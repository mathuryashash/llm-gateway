package middleware

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/mathuryashash/llm-gateway/internal/config"
)

func TestRequestIDHeader(t *testing.T) {
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id, ok := r.Context().Value(CtxRequestID).(string)
		if !ok || id == "" {
			t.Error("request ID not in context")
		}
	})
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	RequestID(next).ServeHTTP(w, r)
	if w.Header().Get("X-Request-Id") == "" {
		t.Fatal("X-Request-Id header missing")
	}
}

func TestRequestIDPreserveIncoming(t *testing.T) {
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.Context().Value(CtxRequestID).(string)
		if id != "incoming-id" {
			t.Errorf("expected incoming-id, got %s", id)
		}
	})
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.Header.Set("X-Request-Id", "incoming-id")
	w := httptest.NewRecorder()
	RequestID(next).ServeHTTP(w, r)
}

func TestMaxBodySizeLarge(t *testing.T) {
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("should not reach handler")
	})
	r := httptest.NewRequest(http.MethodPost, "/", nil)
	r.ContentLength = 10 * 1024 * 1024 // 10 MB
	w := httptest.NewRecorder()
	MaxBodySize(1024)(next).ServeHTTP(w, r)
	if w.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("expected 413, got %d", w.Code)
	}
}

func TestAuthNoKeys(t *testing.T) {
	// No keys configured → allow all.
	called := false
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { called = true })
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	w := httptest.NewRecorder()
	Auth(nil)(next).ServeHTTP(w, r)
	if !called {
		t.Fatal("should have called next when no keys configured")
	}
}

func TestAuthMissingToken(t *testing.T) {
	keys := []config.ClientKey{{Name: "test", HashedKey: "abc", RequestsMin: 60}}
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("should not reach handler")
	})
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	w := httptest.NewRecorder()
	Auth(keys)(next).ServeHTTP(w, r)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", w.Code)
	}
}

func TestAuthBypassHealthz(t *testing.T) {
	keys := []config.ClientKey{{Name: "test", HashedKey: "abc", RequestsMin: 60}}
	called := false
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { called = true })
	r := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	w := httptest.NewRecorder()
	Auth(keys)(next).ServeHTTP(w, r)
	if !called {
		t.Fatal("healthz should bypass auth")
	}
}
