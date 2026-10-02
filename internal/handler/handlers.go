package handler

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/rajeshwari/seat-reservation/internal/metrics"
	"github.com/rajeshwari/seat-reservation/internal/middleware"
	"github.com/rajeshwari/seat-reservation/internal/models"
	"github.com/rajeshwari/seat-reservation/internal/store"
)

type Handler struct {
	store *store.Store
}

func New(s *store.Store) *Handler {
	return &Handler{store: s}
}

// POST /shows
func (h *Handler) CreateShow(w http.ResponseWriter, r *http.Request) {
	var req models.CreateShowRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body", "bad_request")
		return
	}

	if req.Name == "" {
		writeError(w, http.StatusBadRequest, "name is required", "bad_request")
		return
	}
	if len(req.Seats) == 0 {
		writeError(w, http.StatusBadRequest, "seats are required", "bad_request")
		return
	}
	if req.PricePaise < 0 {
		writeError(w, http.StatusBadRequest, "price_paise must be non-negative", "bad_request")
		return
	}

	// Deduplicate seat labels
	seen := make(map[string]bool)
	for _, s := range req.Seats {
		if seen[s] {
			writeError(w, http.StatusBadRequest, fmt.Sprintf("duplicate seat label: %s", s), "bad_request")
			return
		}
		seen[s] = true
	}

	show, err := h.store.CreateShow(r.Context(), req)
	if err != nil {
		slog.Error("create show failed", "error", err)
		writeError(w, http.StatusConflict, err.Error(), "conflict")
		return
	}

	// Update metrics
	metrics.SeatsAvailable.WithLabelValues(show.ID).Set(float64(show.Counts.Available))
	metrics.SeatsConfirmed.WithLabelValues(show.ID).Set(0)

	writeJSON(w, http.StatusCreated, show)
}

// GET /shows/{id}
func (h *Handler) GetShow(w http.ResponseWriter, r *http.Request) {
	showID := chi.URLParam(r, "id")
	if showID == "" {
		writeError(w, http.StatusBadRequest, "show id is required", "bad_request")
		return
	}

	show, err := h.store.GetShow(r.Context(), showID)
	if err != nil {
		if errors.Is(err, store.ErrShowNotFound) {
			writeError(w, http.StatusNotFound, "show not found", "not_found")
			return
		}
		slog.Error("get show failed", "error", err)
		writeError(w, http.StatusInternalServerError, "internal error", "internal")
		return
	}

	writeJSON(w, http.StatusOK, show)
}

// POST /shows/{id}/reserve
func (h *Handler) ReserveSeat(w http.ResponseWriter, r *http.Request) {
	showID := chi.URLParam(r, "id")
	userID := middleware.GetUserID(r.Context())

	var req models.ReserveRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body", "bad_request")
		return
	}

	// Also check for idempotency key in header
	if req.IdempotencyKey == "" {
		req.IdempotencyKey = r.Header.Get("Idempotency-Key")
	}

	if req.IdempotencyKey == "" {
		writeError(w, http.StatusBadRequest, "idempotency_key is required", "bad_request")
		return
	}
	if len(req.Seats) == 0 {
		writeError(w, http.StatusBadRequest, "seats are required", "bad_request")
		return
	}

	// Deduplicate
	seen := make(map[string]bool)
	for _, s := range req.Seats {
		if seen[s] {
			writeError(w, http.StatusBadRequest, fmt.Sprintf("duplicate seat: %s", s), "bad_request")
			return
		}
		seen[s] = true
	}

	// Get show to know price and per-user limit
	show, err := h.store.GetShowBasic(r.Context(), showID)
	if err != nil {
		if errors.Is(err, store.ErrShowNotFound) {
			writeError(w, http.StatusNotFound, "show not found", "not_found")
			return
		}
		slog.Error("get show failed", "error", err)
		writeError(w, http.StatusInternalServerError, "internal error", "internal")
		return
	}

	reservation, isReplay, err := h.store.ReserveSeat(r.Context(), showID, userID, req, show.PricePaise, show.PerUserLimit)
	if err != nil {
		switch {
		case errors.Is(err, store.ErrSeatTaken):
			metrics.ReservationsDeclined.WithLabelValues("seat_taken").Inc()
			writeError(w, http.StatusConflict, "one or more seats are not available", "seat_taken")
		case errors.Is(err, store.ErrPerUserLimit):
			metrics.ReservationsDeclined.WithLabelValues("per_user_limit").Inc()
			writeError(w, http.StatusConflict, fmt.Sprintf("per-user limit of %d seats exceeded", show.PerUserLimit), "per_user_limit")
		case errors.Is(err, store.ErrIdempotencyConflict):
			metrics.ReservationsDeclined.WithLabelValues("idempotency_conflict").Inc()
			writeError(w, http.StatusConflict, "idempotency key already used with different seats", "idempotency_conflict")
		case errors.Is(err, store.ErrSeatNotFound):
			writeError(w, http.StatusBadRequest, "one or more seats do not exist for this show", "seat_not_found")
		default:
			slog.Error("reserve seat failed", "error", err, "show_id", showID, "user_id", userID)
			writeError(w, http.StatusInternalServerError, "internal error", "internal")
		}
		return
	}

	if isReplay {
		metrics.IdempotentReplays.Inc()
		metrics.ReservationsDeclined.WithLabelValues("idempotent_replay").Inc()
		writeJSON(w, http.StatusCreated, reservation)
		return
	}

	// New reservation confirmed
	metrics.ReservationsConfirmed.Inc()
	metrics.SeatsAvailable.WithLabelValues(showID).Sub(float64(len(req.Seats)))
	metrics.SeatsConfirmed.WithLabelValues(showID).Add(float64(len(req.Seats)))

	writeJSON(w, http.StatusCreated, reservation)
}

// POST /reservations/{id}/cancel
func (h *Handler) CancelReservation(w http.ResponseWriter, r *http.Request) {
	reservationID := chi.URLParam(r, "id")
	userID := middleware.GetUserID(r.Context())

	reservation, err := h.store.CancelReservation(r.Context(), reservationID, userID)
	if err != nil {
		switch {
		case errors.Is(err, store.ErrReservationNotFound):
			writeError(w, http.StatusNotFound, "reservation not found", "not_found")
		case errors.Is(err, store.ErrNotOwner):
			writeError(w, http.StatusForbidden, "you can only cancel your own reservations", "forbidden")
		case errors.Is(err, store.ErrAlreadyCancelled):
			writeError(w, http.StatusConflict, "reservation already cancelled", "already_cancelled")
		default:
			slog.Error("cancel reservation failed", "error", err)
			writeError(w, http.StatusInternalServerError, "internal error", "internal")
		}
		return
	}

	// Update metrics
	metrics.SeatsAvailable.WithLabelValues(reservation.ShowID).Add(float64(len(reservation.Seats)))
	metrics.SeatsConfirmed.WithLabelValues(reservation.ShowID).Sub(float64(len(reservation.Seats)))

	writeJSON(w, http.StatusOK, reservation)
}

// GET /health/live
func (h *Handler) Liveness(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "alive", "timestamp": time.Now().UTC().Format(time.RFC3339)})
}

// GET /health/ready
func (h *Handler) Readiness(w http.ResponseWriter, r *http.Request) {
	if err := h.store.Ping(r.Context()); err != nil {
		slog.Error("readiness check failed", "error", err)
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"status": "not ready", "error": "database unreachable"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ready", "timestamp": time.Now().UTC().Format(time.RFC3339)})
}

// POST /auth/token — utility endpoint to generate tokens for testing
func (h *Handler) GenerateToken(w http.ResponseWriter, r *http.Request) {
	var req struct {
		UserID string `json:"user_id"`
		Role   string `json:"role"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body", "bad_request")
		return
	}
	if req.UserID == "" {
		writeError(w, http.StatusBadRequest, "user_id is required", "bad_request")
		return
	}
	if req.Role == "" {
		req.Role = "user"
	}

	token, err := middleware.GenerateToken(req.UserID, req.Role)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to generate token", "internal")
		return
	}

	writeJSON(w, http.StatusOK, map[string]string{"token": token, "user_id": req.UserID, "role": req.Role})
}

// Metrics middleware
func MetricsMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rw := &statusResponseWriter{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rw, r)
		duration := time.Since(start).Seconds()

		path := r.URL.Path
		// Normalize paths with IDs for metrics cardinality control
		rctx := chi.RouteContext(r.Context())
		if rctx != nil && rctx.RoutePattern() != "" {
			path = rctx.RoutePattern()
		}

		status := strconv.Itoa(rw.status)
		metrics.HTTPRequestDuration.WithLabelValues(r.Method, path, status).Observe(duration)
		metrics.HTTPRequestsTotal.WithLabelValues(r.Method, path, status).Inc()
	})
}

type statusResponseWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusResponseWriter) WriteHeader(code int) {
	w.status = code
	w.ResponseWriter.WriteHeader(code)
}

func writeJSON(w http.ResponseWriter, status int, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg, code string) {
	writeJSON(w, status, models.ErrorResponse{Error: msg, Code: code})
}
