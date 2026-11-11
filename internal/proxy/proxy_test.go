package proxy

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/yashash-pushkar/llm-gateway/internal/breaker"
	"github.com/yashash-pushkar/llm-gateway/internal/cache"
	"github.com/yashash-pushkar/llm-gateway/internal/config"
	"github.com/yashash-pushkar/llm-gateway/internal/provider"
	"github.com/yashash-pushkar/llm-gateway/internal/ratelimit"
	"github.com/yashash-pushkar/llm-gateway/internal/router"
	"github.com/yashash-pushkar/llm-gateway/internal/types"
	"log/slog"
	"os"
)

// fakeProvider builds an httptest.Server that behaves like a real upstream.
func fakeProvider(t *testing.T, statusCode int, body string, latency time.Duration) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if latency > 0 {
			time.Sleep(latency)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(statusCode)
		_, _ = fmt.Fprint(w, body)
	}))
}

func okResponse(model string) string {
	r := types.ChatCompletionResponse{
		ID:     "chatcmpl-test",
		Object: "chat.completion",
		Model:  model,
		Choices: []types.ChatCompletionChoice{
			{Index: 0, Message: types.ChatMessage{Role: "assistant", Content: "hello"}, FinishReason: "stop"},
		},
		Usage: types.UsageInfo{PromptTokens: 10, CompletionTokens: 5, TotalTokens: 15},
	}
	b, _ := json.Marshal(r)
	return string(b)
}

func buildHandler(t *testing.T, routes []router.Route) *Handler {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
	cfg := &config.Config{
		Cache:   config.CacheConfig{Enabled: false},
		Breaker: config.BreakerConfig{FailureThreshold: 3, RecoverySec: 30},
	}
	cfg.Breaker.Recovery = 30 * time.Second

	// Mock router that returns provided routes directly.
	r := &mockRouter{routes: routes}
	br := breaker.NewRegistry(breaker.Config{FailureThreshold: 3, Recovery: 30 * time.Second})
	lim := ratelimit.New(nil, true)
	c := cache.New(nil, 0)
	return New(r, br, lim, c, logger, cfg)
}

// mockRouter implements the router interface inline.
type mockRouter struct {
	routes  []router.Route
	aliases []string
}

func (m *mockRouter) Resolve(alias string) ([]router.Route, error) {
	if len(m.routes) == 0 {
		return nil, fmt.Errorf("model %q not found", alias)
	}
	return m.routes, nil
}

func (m *mockRouter) KnownAliases() []string { return m.aliases }

// We need Handler to accept interface not *router.Router.
// Update Handler to use interface.

func chatRequest(model string) *http.Request {
	temp := 0.0
	body := types.ChatCompletionRequest{
		Model:       model,
		Messages:    []types.ChatMessage{{Role: "user", Content: "hi"}},
		Temperature: &temp,
	}
	b, _ := json.Marshal(body)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	return req
}

func TestHandleChatSuccess(t *testing.T) {
	srv := fakeProvider(t, http.StatusOK, okResponse("test-model"), 0)
	defer srv.Close()

	client := provider.New("test", srv.URL, "key", 5*time.Second)
	routes := []router.Route{{Client: client, Model: "test-model", RouteKey: "fast/test"}}
	h := buildHandler(t, routes)

	w := httptest.NewRecorder()
	h.ServeHTTP(w, chatRequest("fast"))
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 got %d, body: %s", w.Code, w.Body.String())
	}
}

func TestHandleChatUnknownModel(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))
	cfg := &config.Config{Cache: config.CacheConfig{Enabled: false}}
	r := &mockRouter{} // no routes
	br := breaker.NewRegistry(breaker.Config{FailureThreshold: 3, Recovery: 30 * time.Second})
	lim := ratelimit.New(nil, true)
	c := cache.New(nil, 0)
	h := New(r, br, lim, c, logger, cfg)

	w := httptest.NewRecorder()
	h.ServeHTTP(w, chatRequest("unknown-model"))
	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404 got %d", w.Code)
	}
}

func TestHandleChatFallback(t *testing.T) {
	// First server returns 429, second returns 200.
	srv1 := fakeProvider(t, http.StatusTooManyRequests,
		`{"error":{"message":"rate limited","type":"rate_limit_error"}}`, 0)
	defer srv1.Close()
	srv2 := fakeProvider(t, http.StatusOK, okResponse("fallback-model"), 0)
	defer srv2.Close()

	c1 := provider.New("primary", srv1.URL, "key", 5*time.Second)
	c2 := provider.New("secondary", srv2.URL, "key", 5*time.Second)
	routes := []router.Route{
		{Client: c1, Model: "model1", RouteKey: "fast/primary"},
		{Client: c2, Model: "model2", RouteKey: "fast/secondary"},
	}
	h := buildHandler(t, routes)

	w := httptest.NewRecorder()
	h.ServeHTTP(w, chatRequest("fast"))
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 after fallback, got %d, body: %s", w.Code, w.Body.String())
	}
	if w.Header().Get("X-Provider") != "secondary" {
		t.Fatalf("expected X-Provider=secondary, got %s", w.Header().Get("X-Provider"))
	}
}

func TestHandleChatAllFail(t *testing.T) {
	srv := fakeProvider(t, http.StatusServiceUnavailable,
		`{"error":{"message":"down","type":"server_error"}}`, 0)
	defer srv.Close()

	c := provider.New("only", srv.URL, "key", 5*time.Second)
	routes := []router.Route{{Client: c, Model: "model", RouteKey: "fast/only"}}
	h := buildHandler(t, routes)

	w := httptest.NewRecorder()
	h.ServeHTTP(w, chatRequest("fast"))
	if w.Code != http.StatusBadGateway {
		t.Fatalf("expected 502 got %d", w.Code)
	}
}

func TestHealthz(t *testing.T) {
	h := buildHandler(t, nil)
	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 got %d", w.Code)
	}
}

func TestModels(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	cfg := &config.Config{}
	r := &mockRouter{aliases: []string{"fast", "smart"}}
	br := breaker.NewRegistry(breaker.Config{FailureThreshold: 3, Recovery: 30 * time.Second})
	lim := ratelimit.New(nil, true)
	c := cache.New(nil, 0)
	h := New(r, br, lim, c, logger, cfg)
	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 got %d", w.Code)
	}
}

func TestBreakerSkipsOpenRoute(t *testing.T) {
	// Two providers; first is circuit-open, second returns 200.
	srv2 := fakeProvider(t, http.StatusOK, okResponse("model2"), 0)
	defer srv2.Close()

	c1 := provider.New("tripped", "http://127.0.0.1:1", "key", 100*time.Millisecond) // unreachable
	c2 := provider.New("fallback", srv2.URL, "key", 5*time.Second)

	routes := []router.Route{
		{Client: c1, Model: "model1", RouteKey: "fast/tripped"},
		{Client: c2, Model: "model2", RouteKey: "fast/fallback"},
	}
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
	cfg := &config.Config{
		Cache:   config.CacheConfig{Enabled: false},
		Breaker: config.BreakerConfig{FailureThreshold: 1, RecoverySec: 60},
	}
	cfg.Breaker.Recovery = 60 * time.Second

	mr := &mockRouter{routes: routes}
	br := breaker.NewRegistry(breaker.Config{FailureThreshold: 1, Recovery: 60 * time.Second})
	lim := ratelimit.New(nil, true)
	ca := cache.New(nil, 0)
	h := New(mr, br, lim, ca, logger, cfg)

	// Trip the first breaker by sending one failing request.
	w := httptest.NewRecorder()
	h.ServeHTTP(w, chatRequest("fast"))
	// The second request should skip the open breaker and use fallback.
	w2 := httptest.NewRecorder()
	h.ServeHTTP(w2, chatRequest("fast"))
	if w2.Code != http.StatusOK {
		t.Fatalf("expected 200 from fallback, got %d body=%s", w2.Code, w2.Body.String())
	}
}
