package main

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/go-chi/chi/v5"
	chimw "github.com/go-chi/chi/v5/middleware"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/rajeshwari/seat-reservation/internal/handler"
	"github.com/rajeshwari/seat-reservation/internal/middleware"
	"github.com/rajeshwari/seat-reservation/internal/store"
)

func main() {
	// Structured JSON logging
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(logger)

	// Database connection
	dbURL := os.Getenv("DATABASE_URL")
	if dbURL == "" {
		dbURL = "postgres://postgres:postgres@localhost:5432/seatreservation?sslmode=disable"
	}

	ctx := context.Background()
	poolConfig, err := pgxpool.ParseConfig(dbURL)
	if err != nil {
		slog.Error("failed to parse database URL", "error", err)
		os.Exit(1)
	}
	poolConfig.MaxConns = 20
	poolConfig.MinConns = 2

	pool, err := pgxpool.NewWithConfig(ctx, poolConfig)
	if err != nil {
		slog.Error("failed to create connection pool", "error", err)
		os.Exit(1)
	}
	defer pool.Close()

	// Verify DB connection
	if err := pool.Ping(ctx); err != nil {
		slog.Error("failed to ping database", "error", err)
		os.Exit(1)
	}
	slog.Info("connected to database")

	// Auto-migrate: create tables if they don't exist
	if err := runMigrations(ctx, pool); err != nil {
		slog.Error("failed to run migrations", "error", err)
		os.Exit(1)
	}

	// Initialize store and handlers
	s := store.New(pool)
	h := handler.New(s)

	// Router
	r := chi.NewRouter()

	// Global middleware
	r.Use(chimw.Recoverer)
	r.Use(middleware.RequestID)
	r.Use(middleware.Logging)
	r.Use(handler.MetricsMiddleware)

	// Health endpoints (no auth)
	r.Get("/health/live", h.Liveness)
	r.Get("/health/ready", h.Readiness)

	// Metrics endpoint (no auth)
	r.Handle("/metrics", promhttp.Handler())

	// Token generation (for testing — no auth required)
	r.Post("/auth/token", h.GenerateToken)

	// Authenticated routes
	r.Group(func(r chi.Router) {
		r.Use(middleware.Auth)

		// Admin routes
		r.Group(func(r chi.Router) {
			r.Use(middleware.AdminOnly)
			r.Post("/shows", h.CreateShow)
		})

		// User routes
		r.Get("/shows/{id}", h.GetShow)
		r.Post("/shows/{id}/reserve", h.ReserveSeat)
		r.Post("/reservations/{id}/cancel", h.CancelReservation)
	})

	// Server
	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}

	srv := &http.Server{
		Addr:         ":" + port,
		Handler:      r,
		ReadTimeout:  10 * time.Second,
		WriteTimeout: 30 * time.Second,
		IdleTimeout:  60 * time.Second,
	}

	// Graceful shutdown
	go func() {
		slog.Info("server starting", "port", port)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			slog.Error("server failed", "error", err)
			os.Exit(1)
		}
	}()

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit

	slog.Info("shutting down server")
	shutdownCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	if err := srv.Shutdown(shutdownCtx); err != nil {
		slog.Error("server shutdown failed", "error", err)
	}

	fmt.Println("server stopped")
}

func runMigrations(ctx context.Context, pool *pgxpool.Pool) error {
	schema := `
	CREATE EXTENSION IF NOT EXISTS "pgcrypto";

	CREATE TABLE IF NOT EXISTS shows (
		id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
		name VARCHAR(255) NOT NULL UNIQUE,
		price_paise BIGINT NOT NULL CHECK (price_paise >= 0),
		per_user_limit INT NOT NULL DEFAULT 4 CHECK (per_user_limit > 0),
		created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
	);

	CREATE TABLE IF NOT EXISTS seats (
		id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
		show_id UUID NOT NULL REFERENCES shows(id) ON DELETE CASCADE,
		label VARCHAR(20) NOT NULL,
		status VARCHAR(20) NOT NULL DEFAULT 'available' CHECK (status IN ('available', 'confirmed')),
		user_id VARCHAR(255),
		reservation_id UUID,
		updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
		UNIQUE(show_id, label)
	);

	CREATE INDEX IF NOT EXISTS idx_seats_show_status ON seats(show_id, status);
	CREATE INDEX IF NOT EXISTS idx_seats_show_user ON seats(show_id, user_id) WHERE user_id IS NOT NULL;

	CREATE TABLE IF NOT EXISTS reservations (
		id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
		show_id UUID NOT NULL REFERENCES shows(id) ON DELETE CASCADE,
		user_id VARCHAR(255) NOT NULL,
		idempotency_key VARCHAR(255) NOT NULL UNIQUE,
		seats TEXT[] NOT NULL,
		amount_paise BIGINT NOT NULL CHECK (amount_paise >= 0),
		status VARCHAR(20) NOT NULL DEFAULT 'confirmed' CHECK (status IN ('confirmed', 'cancelled')),
		created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
	);

	CREATE INDEX IF NOT EXISTS idx_reservations_show_user ON reservations(show_id, user_id, status);
	CREATE INDEX IF NOT EXISTS idx_reservations_idempotency ON reservations(idempotency_key);
	`
	_, err := pool.Exec(ctx, schema)
	if err != nil {
		return fmt.Errorf("exec schema: %w", err)
	}
	slog.Info("database schema applied")
	return nil
}
