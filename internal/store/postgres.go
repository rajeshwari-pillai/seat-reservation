package store

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/rajeshwari/seat-reservation/internal/models"
)

var (
	ErrShowNotFound       = errors.New("show not found")
	ErrSeatTaken          = errors.New("one or more seats are not available")
	ErrPerUserLimit       = errors.New("per-user seat limit exceeded")
	ErrIdempotencyConflict = errors.New("idempotency key used with different seats")
	ErrReservationNotFound = errors.New("reservation not found")
	ErrNotOwner           = errors.New("not the owner of this reservation")
	ErrAlreadyCancelled   = errors.New("reservation already cancelled")
	ErrSeatNotFound       = errors.New("one or more seats do not exist for this show")
)

type Store struct {
	pool *pgxpool.Pool
}

func New(pool *pgxpool.Pool) *Store {
	return &Store{pool: pool}
}

func (s *Store) Ping(ctx context.Context) error {
	return s.pool.Ping(ctx)
}

func (s *Store) CreateShow(ctx context.Context, req models.CreateShowRequest) (*models.ShowDetail, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback(ctx)

	perUserLimit := req.PerUserLimit
	if perUserLimit <= 0 {
		perUserLimit = 4
	}

	var show models.Show
	err = tx.QueryRow(ctx,
		`INSERT INTO shows (name, price_paise, per_user_limit) VALUES ($1, $2, $3)
		 RETURNING id, name, price_paise, per_user_limit, created_at`,
		req.Name, req.PricePaise, perUserLimit,
	).Scan(&show.ID, &show.Name, &show.PricePaise, &show.PerUserLimit, &show.CreatedAt)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			return nil, fmt.Errorf("show with name %q already exists", req.Name)
		}
		return nil, fmt.Errorf("insert show: %w", err)
	}

	// Batch insert seats
	if len(req.Seats) > 0 {
		rows := make([][]interface{}, len(req.Seats))
		for i, label := range req.Seats {
			rows[i] = []interface{}{show.ID, label}
		}
		_, err = tx.CopyFrom(ctx,
			pgx.Identifier{"seats"},
			[]string{"show_id", "label"},
			pgx.CopyFromRows(rows),
		)
		if err != nil {
			return nil, fmt.Errorf("insert seats: %w", err)
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("commit: %w", err)
	}

	return s.GetShow(ctx, show.ID)
}

func (s *Store) GetShow(ctx context.Context, showID string) (*models.ShowDetail, error) {
	var show models.Show
	err := s.pool.QueryRow(ctx,
		`SELECT id, name, price_paise, per_user_limit, created_at FROM shows WHERE id = $1`,
		showID,
	).Scan(&show.ID, &show.Name, &show.PricePaise, &show.PerUserLimit, &show.CreatedAt)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrShowNotFound
		}
		return nil, fmt.Errorf("query show: %w", err)
	}

	rows, err := s.pool.Query(ctx,
		`SELECT label, status, COALESCE(user_id, ''), COALESCE(reservation_id::text, '')
		 FROM seats WHERE show_id = $1 ORDER BY label`,
		showID,
	)
	if err != nil {
		return nil, fmt.Errorf("query seats: %w", err)
	}
	defer rows.Close()

	var seats []models.Seat
	counts := models.SeatCount{}
	for rows.Next() {
		var seat models.Seat
		if err := rows.Scan(&seat.Label, &seat.Status, &seat.UserID, &seat.ReservationID); err != nil {
			return nil, fmt.Errorf("scan seat: %w", err)
		}
		seats = append(seats, seat)
		switch seat.Status {
		case "available":
			counts.Available++
		case "confirmed":
			counts.Confirmed++
		}
		counts.Total++
	}

	return &models.ShowDetail{
		Show:  show,
		Seats: seats,
		Counts: counts,
	}, nil
}

// ReserveSeat handles the full atomic reservation logic.
// Returns (reservation, isIdempotentReplay, error).
func (s *Store) ReserveSeat(ctx context.Context, showID, userID string, req models.ReserveRequest, pricePaise int64, perUserLimit int) (*models.Reservation, bool, error) {
	// Sort seats for deterministic lock ordering to prevent deadlocks
	requestedSeats := make([]string, len(req.Seats))
	copy(requestedSeats, req.Seats)
	sort.Strings(requestedSeats)

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, false, fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback(ctx)

	// Step 1: Advisory lock on (show_id, user_id) to serialize per-user requests
	// This prevents per-user limit races
	_, err = tx.Exec(ctx,
		`SELECT pg_advisory_xact_lock(hashtext($1 || ':' || $2))`,
		showID, userID,
	)
	if err != nil {
		return nil, false, fmt.Errorf("advisory lock: %w", err)
	}

	// Step 2: Check idempotency key
	var existing models.Reservation
	var existingSeats []string
	err = tx.QueryRow(ctx,
		`SELECT id, show_id, user_id, seats, amount_paise, status, created_at
		 FROM reservations WHERE idempotency_key = $1`,
		req.IdempotencyKey,
	).Scan(&existing.ID, &existing.ShowID, &existing.UserID, &existingSeats,
		&existing.AmountPaise, &existing.Status, &existing.CreatedAt)

	if err == nil {
		// Idempotency key exists — check if same request
		sort.Strings(existingSeats)
		if !slicesEqual(existingSeats, requestedSeats) || existing.ShowID != showID {
			return nil, false, ErrIdempotencyConflict
		}
		existing.Seats = existingSeats
		return &existing, true, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return nil, false, fmt.Errorf("check idempotency: %w", err)
	}

	// Step 3: Lock the requested seats in deterministic order
	// Using FOR UPDATE to prevent concurrent modifications
	lockedRows, err := tx.Query(ctx,
		`SELECT label, status FROM seats
		 WHERE show_id = $1 AND label = ANY($2)
		 ORDER BY label
		 FOR UPDATE`,
		showID, requestedSeats,
	)
	if err != nil {
		return nil, false, fmt.Errorf("lock seats: %w", err)
	}

	lockedSeats := make(map[string]string) // label -> status
	for lockedRows.Next() {
		var label, status string
		if err := lockedRows.Scan(&label, &status); err != nil {
			lockedRows.Close()
			return nil, false, fmt.Errorf("scan locked seat: %w", err)
		}
		lockedSeats[label] = status
	}
	lockedRows.Close()

	// Check all requested seats exist
	if len(lockedSeats) != len(requestedSeats) {
		return nil, false, ErrSeatNotFound
	}

	// Check all seats are available (all-or-nothing)
	for _, label := range requestedSeats {
		if lockedSeats[label] != "available" {
			return nil, false, ErrSeatTaken
		}
	}

	// Step 4: Check per-user limit
	var currentCount int
	err = tx.QueryRow(ctx,
		`SELECT COUNT(*) FROM seats
		 WHERE show_id = $1 AND user_id = $2 AND status = 'confirmed'`,
		showID, userID,
	).Scan(&currentCount)
	if err != nil {
		return nil, false, fmt.Errorf("count user seats: %w", err)
	}

	if currentCount+len(requestedSeats) > perUserLimit {
		return nil, false, ErrPerUserLimit
	}

	// Step 5: Insert reservation
	amount := pricePaise * int64(len(requestedSeats))
	var resID string
	var createdAt time.Time
	err = tx.QueryRow(ctx,
		`INSERT INTO reservations (show_id, user_id, idempotency_key, seats, amount_paise, status)
		 VALUES ($1, $2, $3, $4, $5, 'confirmed')
		 RETURNING id, created_at`,
		showID, userID, req.IdempotencyKey, requestedSeats, amount,
	).Scan(&resID, &createdAt)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			// Race condition: another request with same idempotency key committed first
			return nil, false, ErrIdempotencyConflict
		}
		return nil, false, fmt.Errorf("insert reservation: %w", err)
	}

	// Step 6: Update seats atomically
	tag, err := tx.Exec(ctx,
		`UPDATE seats SET status = 'confirmed', user_id = $1, reservation_id = $2, updated_at = NOW()
		 WHERE show_id = $3 AND label = ANY($4) AND status = 'available'`,
		userID, resID, showID, requestedSeats,
	)
	if err != nil {
		return nil, false, fmt.Errorf("update seats: %w", err)
	}
	if tag.RowsAffected() != int64(len(requestedSeats)) {
		// Should not happen due to FOR UPDATE check, but safety net
		return nil, false, ErrSeatTaken
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, false, fmt.Errorf("commit: %w", err)
	}

	return &models.Reservation{
		ID:          resID,
		ShowID:      showID,
		UserID:      userID,
		Seats:       requestedSeats,
		AmountPaise: amount,
		Status:      "confirmed",
		CreatedAt:   createdAt,
	}, false, nil
}

func (s *Store) CancelReservation(ctx context.Context, reservationID, userID string) (*models.Reservation, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback(ctx)

	// Lock the reservation
	var res models.Reservation
	var seats []string
	err = tx.QueryRow(ctx,
		`SELECT id, show_id, user_id, seats, amount_paise, status
		 FROM reservations WHERE id = $1 FOR UPDATE`,
		reservationID,
	).Scan(&res.ID, &res.ShowID, &res.UserID, &seats, &res.AmountPaise, &res.Status)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrReservationNotFound
		}
		return nil, fmt.Errorf("query reservation: %w", err)
	}
	res.Seats = seats

	if res.UserID != userID {
		return nil, ErrNotOwner
	}

	if res.Status == "cancelled" {
		return nil, ErrAlreadyCancelled
	}

	// Cancel the reservation
	_, err = tx.Exec(ctx,
		`UPDATE reservations SET status = 'cancelled' WHERE id = $1`,
		reservationID,
	)
	if err != nil {
		return nil, fmt.Errorf("update reservation: %w", err)
	}

	// Release seats — only release seats that are confirmed to THIS reservation
	_, err = tx.Exec(ctx,
		`UPDATE seats SET status = 'available', user_id = NULL, reservation_id = NULL, updated_at = NOW()
		 WHERE reservation_id = $1 AND status = 'confirmed'`,
		reservationID,
	)
	if err != nil {
		return nil, fmt.Errorf("release seats: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("commit: %w", err)
	}

	res.Status = "cancelled"
	return &res, nil
}

func (s *Store) GetShowBasic(ctx context.Context, showID string) (*models.Show, error) {
	var show models.Show
	err := s.pool.QueryRow(ctx,
		`SELECT id, name, price_paise, per_user_limit, created_at FROM shows WHERE id = $1`,
		showID,
	).Scan(&show.ID, &show.Name, &show.PricePaise, &show.PerUserLimit, &show.CreatedAt)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrShowNotFound
		}
		return nil, fmt.Errorf("query show: %w", err)
	}
	return &show, nil
}

func slicesEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// SeatLabelList converts a slice of strings to a Postgres text array literal
func SeatLabelList(seats []string) string {
	quoted := make([]string, len(seats))
	for i, s := range seats {
		quoted[i] = fmt.Sprintf("%q", s)
	}
	return "{" + strings.Join(quoted, ",") + "}"
}
