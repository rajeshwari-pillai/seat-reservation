package models

import "time"

type Show struct {
	ID           string    `json:"id"`
	Name         string    `json:"name"`
	PricePaise   int64     `json:"price_paise"`
	PerUserLimit int       `json:"per_user_limit"`
	CreatedAt    time.Time `json:"created_at"`
}

type Seat struct {
	Label         string `json:"label"`
	Status        string `json:"status"`
	UserID        string `json:"user_id,omitempty"`
	ReservationID string `json:"reservation_id,omitempty"`
}

type ShowDetail struct {
	Show
	Seats     []Seat    `json:"seats"`
	Counts    SeatCount `json:"counts"`
}

type SeatCount struct {
	Available int `json:"available"`
	Confirmed int `json:"confirmed"`
	Total     int `json:"total"`
}

type Reservation struct {
	ID             string    `json:"reservation_id"`
	ShowID         string    `json:"show_id"`
	UserID         string    `json:"user_id"`
	Seats          []string  `json:"seats"`
	AmountPaise    int64     `json:"amount_paise"`
	Status         string    `json:"status"`
	IdempotencyKey string    `json:"-"`
	CreatedAt      time.Time `json:"created_at,omitempty"`
}

// Request/Response types

type CreateShowRequest struct {
	Name         string   `json:"name"`
	Seats        []string `json:"seats"`
	PricePaise   int64    `json:"price_paise"`
	PerUserLimit int      `json:"per_user_limit,omitempty"`
}

type ReserveRequest struct {
	Seats          []string `json:"seats"`
	IdempotencyKey string   `json:"idempotency_key"`
}

type ErrorResponse struct {
	Error  string `json:"error"`
	Code   string `json:"code,omitempty"`
}
