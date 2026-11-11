// Package middleware provides HTTP middleware for the gateway:
// request ID injection, structured logging, body size limiting,
// and client API key authentication.
package middleware

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/mathuryashash/llm-gateway/internal/config"
	"github.com/mathuryashash/llm-gateway/internal/types"
)

// ctxKey is used for context value keys to avoid collisions.
type ctxKey string

const (
	CtxRequestID  ctxKey = "request_id"
	CtxClientHash ctxKey = "client_hash"
	CtxClientName ctxKey = "client_name"
)

// RequestID injects a unique request ID into the context and response headers.
func RequestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Reuse an incoming header if present (e.g. from a load balancer).
		id := r.Header.Get("X-Request-Id")
		if id == "" {
			// Generate a compact ID: sha256 of time+counter, truncated to 12 chars.
			h := sha256.Sum256([]byte(time.Now().String() + r.RemoteAddr))
			id = hex.EncodeToString(h[:])[:12]
		}
		ctx := context.WithValue(r.Context(), CtxRequestID, id)
		w.Header().Set("X-Request-Id", id)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// Logger logs request completion with structured fields.
// It never logs request bodies or Authorization headers.
func Logger(logger *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()
			rw := &responseWriter{ResponseWriter: w, status: http.StatusOK}
			next.ServeHTTP(rw, r)
			logger.Info("request",
				"method", r.Method,
				"path", r.URL.Path,
				"status", rw.status,
				"duration_ms", time.Since(start).Milliseconds(),
				"request_id", r.Context().Value(CtxRequestID),
				"remote", r.RemoteAddr,
			)
		})
	}
}

// responseWriter wraps http.ResponseWriter to capture the status code.
type responseWriter struct {
	http.ResponseWriter
	status int
}

func (rw *responseWriter) WriteHeader(code int) {
	rw.status = code
	rw.ResponseWriter.WriteHeader(code)
}

// MaxBodySize rejects requests with a body larger than limit bytes.
func MaxBodySize(limit int64) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.ContentLength > limit {
				writeError(w, http.StatusRequestEntityTooLarge, "request body too large", "request_too_large")
				return
			}
			r.Body = http.MaxBytesReader(w, r.Body, limit)
			next.ServeHTTP(w, r)
		})
	}
}

// Auth validates the Bearer token against the configured hashed keys.
// Requests to /healthz, /readyz and /metrics skip authentication.
func Auth(keys []config.ClientKey) func(http.Handler) http.Handler {
	// Pre-build a map for O(1) lookup.
	keyMap := make(map[string]config.ClientKey, len(keys))
	for _, k := range keys {
		keyMap[strings.ToLower(k.HashedKey)] = k
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			path := r.URL.Path
			if path == "/healthz" || path == "/readyz" || path == "/metrics" {
				next.ServeHTTP(w, r)
				return
			}

			// If no keys are configured, allow all (useful for dev/test).
			if len(keys) == 0 {
				next.ServeHTTP(w, r)
				return
			}

			raw := bearerToken(r)
			if raw == "" {
				writeError(w, http.StatusUnauthorized, "missing Authorization header", "missing_auth")
				return
			}
			h := sha256.Sum256([]byte(raw))
			hash := hex.EncodeToString(h[:])
			ck, ok := keyMap[hash]
			if !ok {
				writeError(w, http.StatusUnauthorized, "invalid API key", "invalid_key")
				return
			}
			ctx := context.WithValue(r.Context(), CtxClientHash, hash)
			ctx = context.WithValue(ctx, CtxClientName, ck.Name)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

func bearerToken(r *http.Request) string {
	h := r.Header.Get("Authorization")
	if !strings.HasPrefix(h, "Bearer ") {
		return ""
	}
	return strings.TrimPrefix(h, "Bearer ")
}

// writeError writes an OpenAI-compatible error response.
func writeError(w http.ResponseWriter, code int, msg, errType string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	resp := types.ErrorResponse{
		Error: types.ErrorDetail{Message: msg, Type: errType},
	}
	_ = json.NewEncoder(w).Encode(resp)
}
