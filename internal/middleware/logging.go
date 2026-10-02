package middleware

import (
	"context"
	"log/slog"
	"net/http"
	"time"

	"github.com/google/uuid"
)

const RequestIDKey contextKey = "request_id"

// RequestFields is a mutable struct stored in context so that
// middlewares later in the chain (like Auth) can write fields
// that earlier middlewares (like Logging) read after the handler returns.
type RequestFields struct {
	RequestID string
	UserID    string
}

type requestFieldsKey struct{}

func GetRequestFields(ctx context.Context) *RequestFields {
	rf, _ := ctx.Value(requestFieldsKey{}).(*RequestFields)
	return rf
}

type responseWriter struct {
	http.ResponseWriter
	status int
}

func (rw *responseWriter) WriteHeader(code int) {
	rw.status = code
	rw.ResponseWriter.WriteHeader(code)
}

func RequestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reqID := r.Header.Get("X-Request-ID")
		if reqID == "" {
			reqID = uuid.New().String()
		}
		w.Header().Set("X-Request-ID", reqID)

		// Create mutable fields struct for this request
		rf := &RequestFields{RequestID: reqID}

		ctx := r.Context()
		ctx = context.WithValue(ctx, RequestIDKey, reqID)
		ctx = context.WithValue(ctx, requestFieldsKey{}, rf)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

func Logging(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rw := &responseWriter{ResponseWriter: w, status: http.StatusOK}

		next.ServeHTTP(rw, r)

		// Read from mutable fields — Auth middleware has written user_id by now
		rf := GetRequestFields(r.Context())
		reqID := ""
		userID := ""
		if rf != nil {
			reqID = rf.RequestID
			userID = rf.UserID
		}

		slog.Info("request",
			"method", r.Method,
			"path", r.URL.Path,
			"status", rw.status,
			"duration_ms", time.Since(start).Milliseconds(),
			"request_id", reqID,
			"user_id", userID,
			"remote_addr", r.RemoteAddr,
		)
	})
}
