package store

import (
	"context"
	"fmt"
	"os"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/rajeshwari/seat-reservation/internal/models"
)

var testStore *Store
var testPool *pgxpool.Pool

func TestMain(m *testing.M) {
	dbURL := os.Getenv("TEST_DATABASE_URL")
	if dbURL == "" {
		dbURL = "postgres://postgres:postgres@localhost:5432/seatreservation_test?sslmode=disable"
	}

	ctx := context.Background()
	var err error
	testPool, err = pgxpool.New(ctx, dbURL)
	if err != nil {
		fmt.Printf("Skipping store tests: cannot connect to test DB: %v\n", err)
		os.Exit(0)
	}

	if err := testPool.Ping(ctx); err != nil {
		fmt.Printf("Skipping store tests: cannot ping test DB: %v\n", err)
		os.Exit(0)
	}

	// Apply schema
	schema := `
	CREATE EXTENSION IF NOT EXISTS "pgcrypto";
	DROP TABLE IF EXISTS reservations CASCADE;
	DROP TABLE IF EXISTS seats CASCADE;
	DROP TABLE IF EXISTS shows CASCADE;

	CREATE TABLE shows (
		id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
		name VARCHAR(255) NOT NULL UNIQUE,
		price_paise BIGINT NOT NULL CHECK (price_paise >= 0),
		per_user_limit INT NOT NULL DEFAULT 4 CHECK (per_user_limit > 0),
		created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
	);
	CREATE TABLE seats (
		id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
		show_id UUID NOT NULL REFERENCES shows(id) ON DELETE CASCADE,
		label VARCHAR(20) NOT NULL,
		status VARCHAR(20) NOT NULL DEFAULT 'available' CHECK (status IN ('available', 'confirmed')),
		user_id VARCHAR(255),
		reservation_id UUID,
		updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
		UNIQUE(show_id, label)
	);
	CREATE TABLE reservations (
		id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
		show_id UUID NOT NULL REFERENCES shows(id) ON DELETE CASCADE,
		user_id VARCHAR(255) NOT NULL,
		idempotency_key VARCHAR(255) NOT NULL UNIQUE,
		seats TEXT[] NOT NULL,
		amount_paise BIGINT NOT NULL CHECK (amount_paise >= 0),
		status VARCHAR(20) NOT NULL DEFAULT 'confirmed' CHECK (status IN ('confirmed', 'cancelled')),
		created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
	);`

	if _, err := testPool.Exec(ctx, schema); err != nil {
		fmt.Printf("Skipping store tests: cannot apply schema: %v\n", err)
		os.Exit(0)
	}

	testStore = New(testPool)
	code := m.Run()
	testPool.Close()
	os.Exit(code)
}

func cleanDB(t *testing.T) {
	t.Helper()
	ctx := context.Background()
	testPool.Exec(ctx, "DELETE FROM reservations")
	testPool.Exec(ctx, "DELETE FROM seats")
	testPool.Exec(ctx, "DELETE FROM shows")
}

func createTestShow(t *testing.T, name string, seats []string) *models.ShowDetail {
	t.Helper()
	show, err := testStore.CreateShow(context.Background(), models.CreateShowRequest{
		Name:         name,
		Seats:        seats,
		PricePaise:   25000,
		PerUserLimit: 4,
	})
	if err != nil {
		t.Fatalf("createTestShow failed: %v", err)
	}
	return show
}

func TestCreateShow(t *testing.T) {
	cleanDB(t)
	show := createTestShow(t, "test-show-1", []string{"A1", "A2", "A3"})

	if show.Name != "test-show-1" {
		t.Errorf("expected name=test-show-1, got %s", show.Name)
	}
	if show.PricePaise != 25000 {
		t.Errorf("expected price=25000, got %d", show.PricePaise)
	}
	if show.Counts.Total != 3 {
		t.Errorf("expected 3 total seats, got %d", show.Counts.Total)
	}
	if show.Counts.Available != 3 {
		t.Errorf("expected 3 available seats, got %d", show.Counts.Available)
	}
	if show.Counts.Confirmed != 0 {
		t.Errorf("expected 0 confirmed seats, got %d", show.Counts.Confirmed)
	}
}

func TestCreateShowDuplicateName(t *testing.T) {
	cleanDB(t)
	createTestShow(t, "dup-show", []string{"A1"})

	_, err := testStore.CreateShow(context.Background(), models.CreateShowRequest{
		Name:       "dup-show",
		Seats:      []string{"B1"},
		PricePaise: 10000,
	})
	if err == nil {
		t.Error("expected error for duplicate show name")
	}
}

func TestReserveSingleSeat(t *testing.T) {
	cleanDB(t)
	show := createTestShow(t, "reserve-test", []string{"A1", "A2", "A3"})

	res, isReplay, err := testStore.ReserveSeat(context.Background(), show.ID, "user-1",
		models.ReserveRequest{Seats: []string{"A1"}, IdempotencyKey: "key-1"},
		show.PricePaise, show.PerUserLimit)

	if err != nil {
		t.Fatalf("reserve failed: %v", err)
	}
	if isReplay {
		t.Error("should not be a replay")
	}
	if res.Status != "confirmed" {
		t.Errorf("expected status=confirmed, got %s", res.Status)
	}
	if res.AmountPaise != 25000 {
		t.Errorf("expected amount=25000, got %d", res.AmountPaise)
	}

	// Verify show state
	updated, _ := testStore.GetShow(context.Background(), show.ID)
	if updated.Counts.Available != 2 {
		t.Errorf("expected 2 available, got %d", updated.Counts.Available)
	}
	if updated.Counts.Confirmed != 1 {
		t.Errorf("expected 1 confirmed, got %d", updated.Counts.Confirmed)
	}
}

func TestReserveMultiSeat(t *testing.T) {
	cleanDB(t)
	show := createTestShow(t, "multi-test", []string{"A1", "A2", "A3", "A4"})

	res, _, err := testStore.ReserveSeat(context.Background(), show.ID, "user-1",
		models.ReserveRequest{Seats: []string{"A1", "A2"}, IdempotencyKey: "multi-1"},
		show.PricePaise, show.PerUserLimit)

	if err != nil {
		t.Fatalf("reserve failed: %v", err)
	}
	if res.AmountPaise != 50000 {
		t.Errorf("expected amount=50000 for 2 seats, got %d", res.AmountPaise)
	}
	if len(res.Seats) != 2 {
		t.Errorf("expected 2 seats, got %d", len(res.Seats))
	}
}

func TestNoDoubleSell(t *testing.T) {
	cleanDB(t)
	show := createTestShow(t, "double-sell-test", []string{"A1"})

	// First user gets the seat
	_, _, err := testStore.ReserveSeat(context.Background(), show.ID, "user-1",
		models.ReserveRequest{Seats: []string{"A1"}, IdempotencyKey: "first"},
		show.PricePaise, show.PerUserLimit)
	if err != nil {
		t.Fatalf("first reserve failed: %v", err)
	}

	// Second user tries same seat
	_, _, err = testStore.ReserveSeat(context.Background(), show.ID, "user-2",
		models.ReserveRequest{Seats: []string{"A1"}, IdempotencyKey: "second"},
		show.PricePaise, show.PerUserLimit)
	if err != ErrSeatTaken {
		t.Errorf("expected ErrSeatTaken, got %v", err)
	}
}

func TestAllOrNothing(t *testing.T) {
	cleanDB(t)
	show := createTestShow(t, "all-or-nothing", []string{"A1", "A2", "A3"})

	// Take A2
	testStore.ReserveSeat(context.Background(), show.ID, "user-1",
		models.ReserveRequest{Seats: []string{"A2"}, IdempotencyKey: "take-a2"},
		show.PricePaise, show.PerUserLimit)

	// Try to get A1+A2 — should fail entirely (A2 is taken)
	_, _, err := testStore.ReserveSeat(context.Background(), show.ID, "user-2",
		models.ReserveRequest{Seats: []string{"A1", "A2"}, IdempotencyKey: "all-or-nothing"},
		show.PricePaise, show.PerUserLimit)
	if err != ErrSeatTaken {
		t.Errorf("expected ErrSeatTaken for all-or-nothing, got %v", err)
	}

	// A1 should still be available
	updated, _ := testStore.GetShow(context.Background(), show.ID)
	for _, seat := range updated.Seats {
		if seat.Label == "A1" && seat.Status != "available" {
			t.Errorf("A1 should still be available after all-or-nothing failure, got %s", seat.Status)
		}
	}
}

func TestIdempotentReplay(t *testing.T) {
	cleanDB(t)
	show := createTestShow(t, "idempotent-test", []string{"A1", "A2"})

	// First request
	res1, replay1, err := testStore.ReserveSeat(context.Background(), show.ID, "user-1",
		models.ReserveRequest{Seats: []string{"A1"}, IdempotencyKey: "idem-1"},
		show.PricePaise, show.PerUserLimit)
	if err != nil {
		t.Fatalf("first reserve failed: %v", err)
	}
	if replay1 {
		t.Error("first should not be replay")
	}

	// Same key, same seats — should return same reservation
	res2, replay2, err := testStore.ReserveSeat(context.Background(), show.ID, "user-1",
		models.ReserveRequest{Seats: []string{"A1"}, IdempotencyKey: "idem-1"},
		show.PricePaise, show.PerUserLimit)
	if err != nil {
		t.Fatalf("replay failed: %v", err)
	}
	if !replay2 {
		t.Error("second should be replay")
	}
	if res1.ID != res2.ID {
		t.Errorf("replay should return same reservation ID: %s vs %s", res1.ID, res2.ID)
	}

	// Only 1 seat should be consumed
	updated, _ := testStore.GetShow(context.Background(), show.ID)
	if updated.Counts.Confirmed != 1 {
		t.Errorf("expected 1 confirmed after idempotent replay, got %d", updated.Counts.Confirmed)
	}
}

func TestIdempotencyConflict(t *testing.T) {
	cleanDB(t)
	show := createTestShow(t, "conflict-test", []string{"A1", "A2"})

	// First request with key
	testStore.ReserveSeat(context.Background(), show.ID, "user-1",
		models.ReserveRequest{Seats: []string{"A1"}, IdempotencyKey: "conflict-key"},
		show.PricePaise, show.PerUserLimit)

	// Same key, different seats
	_, _, err := testStore.ReserveSeat(context.Background(), show.ID, "user-1",
		models.ReserveRequest{Seats: []string{"A2"}, IdempotencyKey: "conflict-key"},
		show.PricePaise, show.PerUserLimit)
	if err != ErrIdempotencyConflict {
		t.Errorf("expected ErrIdempotencyConflict, got %v", err)
	}
}

func TestPerUserLimit(t *testing.T) {
	cleanDB(t)
	show := createTestShow(t, "limit-test", []string{"A1", "A2", "A3", "A4", "A5", "A6"})

	// Book 4 seats (the limit)
	for i := 1; i <= 4; i++ {
		_, _, err := testStore.ReserveSeat(context.Background(), show.ID, "user-1",
			models.ReserveRequest{
				Seats:          []string{fmt.Sprintf("A%d", i)},
				IdempotencyKey: fmt.Sprintf("limit-key-%d", i),
			},
			show.PricePaise, show.PerUserLimit)
		if err != nil {
			t.Fatalf("reserve seat A%d failed: %v", i, err)
		}
	}

	// 5th seat should fail
	_, _, err := testStore.ReserveSeat(context.Background(), show.ID, "user-1",
		models.ReserveRequest{Seats: []string{"A5"}, IdempotencyKey: "limit-key-5"},
		show.PricePaise, show.PerUserLimit)
	if err != ErrPerUserLimit {
		t.Errorf("expected ErrPerUserLimit, got %v", err)
	}
}

func TestCancelReservation(t *testing.T) {
	cleanDB(t)
	show := createTestShow(t, "cancel-test", []string{"A1", "A2"})

	res, _, _ := testStore.ReserveSeat(context.Background(), show.ID, "user-1",
		models.ReserveRequest{Seats: []string{"A1"}, IdempotencyKey: "cancel-1"},
		show.PricePaise, show.PerUserLimit)

	// Cancel it
	cancelled, err := testStore.CancelReservation(context.Background(), res.ID, "user-1")
	if err != nil {
		t.Fatalf("cancel failed: %v", err)
	}
	if cancelled.Status != "cancelled" {
		t.Errorf("expected status=cancelled, got %s", cancelled.Status)
	}

	// Seat should be available again
	updated, _ := testStore.GetShow(context.Background(), show.ID)
	if updated.Counts.Available != 2 {
		t.Errorf("expected 2 available after cancel, got %d", updated.Counts.Available)
	}
}

func TestCancelNotOwner(t *testing.T) {
	cleanDB(t)
	show := createTestShow(t, "not-owner-test", []string{"A1"})

	res, _, _ := testStore.ReserveSeat(context.Background(), show.ID, "user-1",
		models.ReserveRequest{Seats: []string{"A1"}, IdempotencyKey: "owner-1"},
		show.PricePaise, show.PerUserLimit)

	// Another user tries to cancel
	_, err := testStore.CancelReservation(context.Background(), res.ID, "user-2")
	if err != ErrNotOwner {
		t.Errorf("expected ErrNotOwner, got %v", err)
	}
}

func TestCancelledSeatRebookable(t *testing.T) {
	cleanDB(t)
	show := createTestShow(t, "rebook-test", []string{"A1"})

	// Book then cancel
	res, _, _ := testStore.ReserveSeat(context.Background(), show.ID, "user-1",
		models.ReserveRequest{Seats: []string{"A1"}, IdempotencyKey: "rebook-1"},
		show.PricePaise, show.PerUserLimit)
	testStore.CancelReservation(context.Background(), res.ID, "user-1")

	// Another user should be able to book it
	res2, _, err := testStore.ReserveSeat(context.Background(), show.ID, "user-2",
		models.ReserveRequest{Seats: []string{"A1"}, IdempotencyKey: "rebook-2"},
		show.PricePaise, show.PerUserLimit)
	if err != nil {
		t.Fatalf("rebook failed: %v", err)
	}
	if res2.UserID != "user-2" {
		t.Errorf("expected user-2, got %s", res2.UserID)
	}
}

func TestSeatNotFound(t *testing.T) {
	cleanDB(t)
	show := createTestShow(t, "notfound-test", []string{"A1"})

	_, _, err := testStore.ReserveSeat(context.Background(), show.ID, "user-1",
		models.ReserveRequest{Seats: []string{"Z99"}, IdempotencyKey: "notfound-1"},
		show.PricePaise, show.PerUserLimit)
	if err != ErrSeatNotFound {
		t.Errorf("expected ErrSeatNotFound, got %v", err)
	}
}

func TestReconciliationInvariant(t *testing.T) {
	cleanDB(t)
	show := createTestShow(t, "invariant-test", []string{"A1", "A2", "A3", "A4", "A5"})

	// Book some seats
	testStore.ReserveSeat(context.Background(), show.ID, "user-1",
		models.ReserveRequest{Seats: []string{"A1"}, IdempotencyKey: "inv-1"},
		show.PricePaise, show.PerUserLimit)
	testStore.ReserveSeat(context.Background(), show.ID, "user-2",
		models.ReserveRequest{Seats: []string{"A2", "A3"}, IdempotencyKey: "inv-2"},
		show.PricePaise, show.PerUserLimit)

	updated, _ := testStore.GetShow(context.Background(), show.ID)
	total := updated.Counts.Available + updated.Counts.Confirmed
	if total != updated.Counts.Total {
		t.Errorf("reconciliation invariant violated: %d + %d != %d",
			updated.Counts.Available, updated.Counts.Confirmed, updated.Counts.Total)
	}
}

func TestConcurrentHotSeat(t *testing.T) {
	cleanDB(t)
	show := createTestShow(t, "concurrent-test", []string{"A1"})

	winners := 0
	losers := 0
	var mu sync.Mutex
	var wg sync.WaitGroup

	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			_, _, err := testStore.ReserveSeat(context.Background(), show.ID,
				fmt.Sprintf("user-%d", idx),
				models.ReserveRequest{
					Seats:          []string{"A1"},
					IdempotencyKey: fmt.Sprintf("concurrent-%d", idx),
				},
				show.PricePaise, show.PerUserLimit)

			mu.Lock()
			defer mu.Unlock()
			if err == nil {
				winners++
			} else if err == ErrSeatTaken {
				losers++
			} else {
				t.Errorf("unexpected error: %v", err)
			}
		}(i)
	}

	wg.Wait()

	if winners != 1 {
		t.Errorf("expected exactly 1 winner, got %d", winners)
	}
	if losers != 49 {
		t.Errorf("expected 49 losers, got %d", losers)
	}

	// Invariant check
	updated, _ := testStore.GetShow(context.Background(), show.ID)
	if updated.Counts.Confirmed != 1 {
		t.Errorf("expected 1 confirmed, got %d", updated.Counts.Confirmed)
	}
	if updated.Counts.Available != 0 {
		t.Errorf("expected 0 available, got %d", updated.Counts.Available)
	}
}

func TestConcurrentPerUserLimit(t *testing.T) {
	cleanDB(t)
	seats := []string{"A1", "A2", "A3", "A4", "A5", "A6", "A7", "A8", "A9", "A10"}
	show := createTestShow(t, "concurrent-limit-test", seats)

	// Same user fires 10 parallel requests, limit is 4
	var confirmed int
	var mu sync.Mutex
	var wg sync.WaitGroup

	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			_, _, err := testStore.ReserveSeat(context.Background(), show.ID, "same-user",
				models.ReserveRequest{
					Seats:          []string{seats[idx]},
					IdempotencyKey: fmt.Sprintf("limit-concurrent-%d", idx),
				},
				show.PricePaise, show.PerUserLimit)

			mu.Lock()
			defer mu.Unlock()
			if err == nil {
				confirmed++
			}
		}(i)
	}

	wg.Wait()

	if confirmed > 4 {
		t.Errorf("per-user limit violated: user got %d seats (limit 4)", confirmed)
	}
}
