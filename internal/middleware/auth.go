package middleware

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"strings"
	"time"
)

type contextKey string

const (
	UserIDKey contextKey = "user_id"
	RoleKey   contextKey = "role"
)

// Simple JWT-like token: base64(header).base64(payload).base64(signature)
// Payload: {"sub": "user123", "role": "user", "exp": 1234567890}

var SigningKey = []byte("seat-reservation-secret-key-change-in-prod")

type TokenPayload struct {
	Sub  string `json:"sub"`
	Role string `json:"role"`
	Exp  int64  `json:"exp"`
}

func GenerateToken(userID, role string) (string, error) {
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"HS256","typ":"JWT"}`))

	payload := TokenPayload{
		Sub:  userID,
		Role: role,
		Exp:  time.Now().Add(24 * time.Hour).Unix(),
	}
	payloadBytes, err := json.Marshal(payload)
	if err != nil {
		return "", err
	}
	payloadB64 := base64.RawURLEncoding.EncodeToString(payloadBytes)

	sigInput := header + "." + payloadB64
	mac := hmac.New(sha256.New, SigningKey)
	mac.Write([]byte(sigInput))
	sig := base64.RawURLEncoding.EncodeToString(mac.Sum(nil))

	return sigInput + "." + sig, nil
}

func Auth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		authHeader := r.Header.Get("Authorization")
		if authHeader == "" {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "missing Authorization header"})
			return
		}

		if !strings.HasPrefix(authHeader, "Bearer ") {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "invalid Authorization format"})
			return
		}

		token := strings.TrimPrefix(authHeader, "Bearer ")
		parts := strings.Split(token, ".")
		if len(parts) != 3 {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "invalid token format"})
			return
		}

		// Verify signature
		sigInput := parts[0] + "." + parts[1]
		mac := hmac.New(sha256.New, SigningKey)
		mac.Write([]byte(sigInput))
		expectedSig := base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
		if !hmac.Equal([]byte(parts[2]), []byte(expectedSig)) {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "invalid token signature"})
			return
		}

		// Decode payload
		payloadBytes, err := base64.RawURLEncoding.DecodeString(parts[1])
		if err != nil {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "invalid token payload"})
			return
		}

		var payload TokenPayload
		if err := json.Unmarshal(payloadBytes, &payload); err != nil {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "invalid token payload"})
			return
		}

		if payload.Exp < time.Now().Unix() {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "token expired"})
			return
		}

		ctx := context.WithValue(r.Context(), UserIDKey, payload.Sub)
		ctx = context.WithValue(ctx, RoleKey, payload.Role)

		// Write user_id to shared mutable fields so Logging middleware can read it
		if rf := GetRequestFields(ctx); rf != nil {
			rf.UserID = payload.Sub
		}

		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

func AdminOnly(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		role, _ := r.Context().Value(RoleKey).(string)
		if role != "admin" {
			writeJSON(w, http.StatusForbidden, map[string]string{"error": "admin access required"})
			return
		}
		next.ServeHTTP(w, r)
	})
}

func GetUserID(ctx context.Context) string {
	uid, _ := ctx.Value(UserIDKey).(string)
	return uid
}

func writeJSON(w http.ResponseWriter, status int, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}
