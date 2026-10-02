package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const (
	numUsers       = 500   // number of distinct users
	hotSeats       = 5     // number of "hot" seats everyone fights over
	totalSeats     = 100   // total seats in the show
	perUserLimit   = 4
	retryDuplicates = true // some users retry with same idempotency key
)

type result struct {
	Status int
	Body   string
	Err    error
}

func main() {
	baseURL := "http://localhost:8080"
	if len(os.Args) > 1 {
		baseURL = strings.TrimRight(os.Args[1], "/")
	}

	fmt.Printf("=== Seat Reservation Burst Test ===\n")
	fmt.Printf("Target: %s\n", baseURL)
	fmt.Printf("Users: %d | Hot seats: %d | Total seats: %d\n\n", numUsers, hotSeats, totalSeats)

	client := &http.Client{
		Timeout: 30 * time.Second,
		Transport: &http.Transport{
			MaxIdleConns:        1000,
			MaxIdleConnsPerHost: 1000,
			MaxConnsPerHost:     1000,
		},
	}

	// Step 1: Get admin token
	fmt.Println("[1/5] Getting admin token...")
	adminToken := getToken(client, baseURL, "admin-user", "admin")

	// Step 2: Create show with seats
	fmt.Println("[2/5] Creating show...")
	seatLabels := make([]string, totalSeats)
	for i := 0; i < totalSeats; i++ {
		row := string(rune('A' + i/20))
		num := (i % 20) + 1
		seatLabels[i] = fmt.Sprintf("%s%d", row, num)
	}

	showID := createShow(client, baseURL, adminToken, seatLabels)
	fmt.Printf("   Show created: %s (%d seats)\n", showID, totalSeats)

	// Step 3: Generate user tokens
	fmt.Println("[3/5] Generating user tokens...")
	userTokens := make([]string, numUsers)
	for i := 0; i < numUsers; i++ {
		userTokens[i] = getToken(client, baseURL, fmt.Sprintf("user-%d", i), "user")
	}

	// Step 4: Build requests
	fmt.Println("[4/5] Building burst requests...")

	type request struct {
		userIdx        int
		token          string
		seats          []string
		idempotencyKey string
	}

	var requests []request

	// Hot seat storm: all users try for the first N hot seats
	for i := 0; i < numUsers; i++ {
		for j := 0; j < hotSeats; j++ {
			key := fmt.Sprintf("user-%d-hot-%s", i, seatLabels[j])
			requests = append(requests, request{
				userIdx:        i,
				token:          userTokens[i],
				seats:          []string{seatLabels[j]},
				idempotencyKey: key,
			})
		}
	}

	// Some idempotent retries (same key, same seats)
	for i := 0; i < 200; i++ {
		idx := i % numUsers
		key := fmt.Sprintf("user-%d-hot-%s", idx, seatLabels[0])
		requests = append(requests, request{
			userIdx:        idx,
			token:          userTokens[idx],
			seats:          []string{seatLabels[0]},
			idempotencyKey: key,
		})
	}

	// Some idempotent conflicts (same key, different seats)
	for i := 0; i < 50; i++ {
		idx := i % numUsers
		key := fmt.Sprintf("user-%d-hot-%s", idx, seatLabels[0])
		requests = append(requests, request{
			userIdx:        idx,
			token:          userTokens[idx],
			seats:          []string{seatLabels[hotSeats]}, // different seat, same key
			idempotencyKey: key,
		})
	}

	// Per-user limit test: some users try to book many seats
	for i := 0; i < 20; i++ {
		for j := hotSeats; j < hotSeats+perUserLimit+3; j++ {
			key := fmt.Sprintf("user-%d-spread-%s", i, seatLabels[j])
			requests = append(requests, request{
				userIdx:        i,
				token:          userTokens[i],
				seats:          []string{seatLabels[j]},
				idempotencyKey: key,
			})
		}
	}

	fmt.Printf("   Total requests to fire: %d\n", len(requests))

	// Step 5: Fire burst
	fmt.Println("[5/5] Firing burst...")
	start := time.Now()

	var (
		confirmed          int64
		idempotentReplay   int64
		seatTaken          int64
		perUserLimitHit    int64
		idempotentConflict int64
		serverErrors       int64
		otherErrors        int64
		networkErrors      int64
		mu                 sync.Mutex
		statusCounts       = make(map[int]int)
		seenReservations   = make(map[string]bool)
	)

	var wg sync.WaitGroup
	semaphore := make(chan struct{}, 200) // max concurrency

	for _, req := range requests {
		wg.Add(1)
		go func(r request) {
			defer wg.Done()
			semaphore <- struct{}{}
			defer func() { <-semaphore }()

			body, _ := json.Marshal(map[string]interface{}{
				"seats":          r.seats,
				"idempotency_key": r.idempotencyKey,
			})

			httpReq, _ := http.NewRequest("POST",
				fmt.Sprintf("%s/shows/%s/reserve", baseURL, showID),
				bytes.NewReader(body))
			httpReq.Header.Set("Content-Type", "application/json")
			httpReq.Header.Set("Authorization", "Bearer "+r.token)

			resp, err := client.Do(httpReq)
			if err != nil {
				atomic.AddInt64(&networkErrors, 1)
				return
			}
			defer resp.Body.Close()
			respBody, _ := io.ReadAll(resp.Body)

			mu.Lock()
			statusCounts[resp.StatusCode]++
			mu.Unlock()

			switch resp.StatusCode {
			case 201:
				var respData map[string]interface{}
				json.Unmarshal(respBody, &respData)
				resID, _ := respData["reservation_id"].(string)

				mu.Lock()
				if seenReservations[resID] {
					mu.Unlock()
					atomic.AddInt64(&idempotentReplay, 1)
				} else {
					seenReservations[resID] = true
					mu.Unlock()
					atomic.AddInt64(&confirmed, 1)
				}
			case 409:
				var errResp map[string]string
				json.Unmarshal(respBody, &errResp)
				switch errResp["code"] {
				case "seat_taken":
					atomic.AddInt64(&seatTaken, 1)
				case "per_user_limit":
					atomic.AddInt64(&perUserLimitHit, 1)
				case "idempotency_conflict":
					atomic.AddInt64(&idempotentConflict, 1)
				default:
					atomic.AddInt64(&otherErrors, 1)
				}
			default:
				if resp.StatusCode >= 500 {
					atomic.AddInt64(&serverErrors, 1)
				} else {
					atomic.AddInt64(&otherErrors, 1)
				}
			}
		}(req)
	}

	wg.Wait()
	duration := time.Since(start)

	fmt.Printf("\n=== Results (%s) ===\n", duration.Round(time.Millisecond))
	fmt.Printf("Total requests:        %d\n", len(requests))
	fmt.Printf("Confirmed:             %d\n", confirmed)
	fmt.Printf("Idempotent replay:     %d\n", idempotentReplay)
	fmt.Printf("Seat taken (409):      %d\n", seatTaken)
	fmt.Printf("Per-user limit (409):  %d\n", perUserLimitHit)
	fmt.Printf("Idempotent conflict:   %d\n", idempotentConflict)
	fmt.Printf("Server errors (5xx):   %d\n", serverErrors)
	fmt.Printf("Network errors:        %d\n", networkErrors)
	fmt.Printf("Other:                 %d\n", otherErrors)

	fmt.Printf("\nStatus code distribution:\n")
	codes := make([]int, 0)
	for code := range statusCounts {
		codes = append(codes, code)
	}
	sort.Ints(codes)
	for _, code := range codes {
		fmt.Printf("  %d: %d\n", code, statusCounts[code])
	}

	// Step 6: Reconciliation check
	fmt.Printf("\n=== Reconciliation Check ===\n")
	httpReq, _ := http.NewRequest("GET", fmt.Sprintf("%s/shows/%s", baseURL, showID), nil)
	httpReq.Header.Set("Authorization", "Bearer "+userTokens[0])
	resp, err := client.Do(httpReq)
	if err != nil {
		fmt.Printf("ERROR: failed to get show state: %v\n", err)
		os.Exit(1)
	}
	defer resp.Body.Close()

	var showState struct {
		Counts struct {
			Available int `json:"available"`
			Confirmed int `json:"confirmed"`
			Total     int `json:"total"`
		} `json:"counts"`
		Seats []struct {
			Label  string `json:"label"`
			Status string `json:"status"`
			UserID string `json:"user_id"`
		} `json:"seats"`
	}
	json.NewDecoder(resp.Body).Decode(&showState)

	fmt.Printf("Available: %d\n", showState.Counts.Available)
	fmt.Printf("Confirmed: %d\n", showState.Counts.Confirmed)
	fmt.Printf("Total:     %d\n", showState.Counts.Total)

	invariantHolds := showState.Counts.Available+showState.Counts.Confirmed == showState.Counts.Total
	fmt.Printf("Invariant (available + confirmed == total): %v\n", invariantHolds)

	// Check no double-sells
	seatOwners := make(map[string]string)
	doubleSells := 0
	for _, seat := range showState.Seats {
		if seat.Status == "confirmed" {
			if _, exists := seatOwners[seat.Label]; exists {
				doubleSells++
			}
			seatOwners[seat.Label] = seat.UserID
		}
	}
	fmt.Printf("Double-sells detected: %d\n", doubleSells)

	// Check per-user limit
	userSeatCounts := make(map[string]int)
	for _, seat := range showState.Seats {
		if seat.Status == "confirmed" && seat.UserID != "" {
			userSeatCounts[seat.UserID]++
		}
	}
	limitViolations := 0
	for _, count := range userSeatCounts {
		if count > perUserLimit {
			limitViolations++
		}
	}
	fmt.Printf("Per-user limit violations: %d\n", limitViolations)

	fmt.Printf("\n=== VERDICT ===\n")
	if invariantHolds && doubleSells == 0 && serverErrors == 0 && limitViolations == 0 {
		fmt.Println("PASS: All correctness checks passed!")
	} else {
		fmt.Println("FAIL: Correctness issues detected")
		if !invariantHolds {
			fmt.Println("  - Reconciliation invariant violated")
		}
		if doubleSells > 0 {
			fmt.Println("  - Double-sells detected")
		}
		if serverErrors > 0 {
			fmt.Println("  - Server errors occurred")
		}
		if limitViolations > 0 {
			fmt.Println("  - Per-user limit violations")
		}
		os.Exit(1)
	}
}

func getToken(client *http.Client, baseURL, userID, role string) string {
	body, _ := json.Marshal(map[string]string{"user_id": userID, "role": role})
	resp, err := client.Post(baseURL+"/auth/token", "application/json", bytes.NewReader(body))
	if err != nil {
		fmt.Printf("ERROR getting token: %v\n", err)
		os.Exit(1)
	}
	defer resp.Body.Close()
	var result map[string]string
	json.NewDecoder(resp.Body).Decode(&result)
	return result["token"]
}

func createShow(client *http.Client, baseURL, token string, seats []string) string {
	body, _ := json.Marshal(map[string]interface{}{
		"name":           fmt.Sprintf("burst-test-%d", time.Now().UnixNano()),
		"seats":          seats,
		"price_paise":    25000,
		"per_user_limit": perUserLimit,
	})
	req, _ := http.NewRequest("POST", baseURL+"/shows", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)

	resp, err := client.Do(req)
	if err != nil {
		fmt.Printf("ERROR creating show: %v\n", err)
		os.Exit(1)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 201 {
		respBody, _ := io.ReadAll(resp.Body)
		fmt.Printf("ERROR creating show (status %d): %s\n", resp.StatusCode, string(respBody))
		os.Exit(1)
	}

	var result map[string]interface{}
	json.NewDecoder(resp.Body).Decode(&result)
	return result["id"].(string)
}
