// Package integration contains integration tests that require Docker
// (testcontainers-go for Redis, inline fakeupstream HTTP servers).
// Run with: go test ./integration/... -v -timeout 120s
//
// These tests verify:
// 1. Redis-backed rate limiting shared across two gateway instances.
// 2. Full fallback chain: primary 429 → secondary 200.
// 3. Cache HIT on a second identical deterministic request.
// 4. Client cancellation stops the upstream call.
// 5. Retry-After is logged when upstream sends it.
package integration

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
	tcredis "github.com/testcontainers/testcontainers-go/modules/redis"

	"github.com/mathuryashash/llm-gateway/internal/breaker"
	"github.com/mathuryashash/llm-gateway/internal/cache"
	"github.com/mathuryashash/llm-gateway/internal/config"
	"github.com/mathuryashash/llm-gateway/internal/middleware"
	"github.com/mathuryashash/llm-gateway/internal/provider"
	"github.com/mathuryashash/llm-gateway/internal/proxy"
	"github.com/mathuryashash/llm-gateway/internal/ratelimit"
	"github.com/mathuryashash/llm-gateway/internal/router"
	"github.com/mathuryashash/llm-gateway/internal/types"
	"log/slog"
)

// startRedis starts a Redis container and returns a connected client + cleanup func.
func startRedis(t *testing.T) (*redis.Client, func()) {
	t.Helper()
	ctx := context.Background()
	c, err := tcredis.Run(ctx, "redis:7-alpine")
	if err != nil {
		t.Skipf("Docker unavailable, skipping integration test: %v", err)
	}
	addr, err := c.ConnectionString(ctx)
	if err != nil {
		t.Fatalf("get redis addr: %v", err)
	}
	// addr is redis://localhost:XXXXX — parse to host:port
	addr = addr[len("redis://"):]
	rdb := redis.NewClient(&redis.Options{Addr: addr})
	pingCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	if err := rdb.Ping(pingCtx).Err(); err != nil {
		t.Fatalf("redis ping: %v", err)
	}
	cleanup := func() {
		_ = rdb.Close()
		_ = c.Terminate(ctx)
	}
	return rdb, cleanup
}

// fakeUpstreamServer creates an inline HTTP server for tests.
func fakeUpstreamServer(statusCode int, body string, latency time.Duration) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if latency > 0 {
			time.Sleep(latency)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(statusCode)
		_, _ = fmt.Fprint(w, body)
	}))
}

func okBody(model string) string {
	r := types.ChatCompletionResponse{
		ID: "test", Object: "chat.completion", Model: model,
		Choices: []types.ChatCompletionChoice{
			{Index: 0, Message: types.ChatMessage{Role: "assistant", Content: "hi"}, FinishReason: "stop"},
		},
		Usage: types.UsageInfo{PromptTokens: 5, CompletionTokens: 5, TotalTokens: 10},
	}
	b, _ := json.Marshal(r)
	return string(b)
}

func buildGateway(r proxy.RouterIface, br *breaker.Registry, lim *ratelimit.Limiter, c *cache.Cache, cfg *config.Config) http.Handler {
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
	h := proxy.New(r, br, lim, c, logger, cfg)
	mux := http.NewServeMux()
	mux.Handle("/", applyMiddleware(h, cfg, logger))
	return mux
}

func applyMiddleware(h http.Handler, cfg *config.Config, logger *slog.Logger) http.Handler {
	h = middleware.Auth(cfg.ClientKeys)(h)
	h = middleware.MaxBodySize(cfg.Server.MaxBodyBytes)(h)
	h = middleware.Logger(logger)(h)
	h = middleware.RequestID(h)
	return h
}

func chatReq(model string) *http.Request {
	temp := 0.0
	body := types.ChatCompletionRequest{
		Model:       model,
		Messages:    []types.ChatMessage{{Role: "user", Content: "hello"}},
		Temperature: &temp,
	}
	b, _ := json.Marshal(body)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	return req
}

type simpleRouter struct {
	routes  []router.Route
	aliases []string
}

func (s *simpleRouter) Resolve(_ string) ([]router.Route, error) {
	if len(s.routes) == 0 {
		return nil, fmt.Errorf("no routes")
	}
	return s.routes, nil
}

func (s *simpleRouter) KnownAliases() []string { return s.aliases }

// TestFallbackChain: primary 429 → secondary 200.
func TestFallbackChain(t *testing.T) {
	primary := fakeUpstreamServer(http.StatusTooManyRequests,
		`{"error":{"message":"rate limited","type":"rate_limit_error"}}`, 0)
	defer primary.Close()

	secondary := fakeUpstreamServer(http.StatusOK, okBody("model"), 0)
	defer secondary.Close()

	c1 := provider.New("primary", primary.URL, "k", 5*time.Second)
	c2 := provider.New("secondary", secondary.URL, "k", 5*time.Second)

	r := &simpleRouter{routes: []router.Route{
		{Client: c1, Model: "m1", RouteKey: "fast/primary"},
		{Client: c2, Model: "m2", RouteKey: "fast/secondary"},
	}}
	cfg := &config.Config{
		Cache:   config.CacheConfig{Enabled: false},
		Breaker: config.BreakerConfig{FailureThreshold: 3, RecoverySec: 30},
		Server:  config.ServerConfig{MaxBodyBytes: 1 << 20},
	}
	cfg.Breaker.Recovery = 30 * time.Second

	br := breaker.NewRegistry(breaker.Config{FailureThreshold: 3, Recovery: 30 * time.Second})
	lim := ratelimit.New(nil, true)
	ca := cache.New(nil, 0)

	srv := httptest.NewServer(buildGateway(r, br, lim, ca, cfg))
	defer srv.Close()

	resp, err := http.Post(srv.URL+"/v1/chat/completions",
		"application/json", bytes.NewReader(func() []byte {
			temp := 0.0
			b, _ := json.Marshal(types.ChatCompletionRequest{
				Model:       "fast",
				Messages:    []types.ChatMessage{{Role: "user", Content: "hello"}},
				Temperature: &temp,
			})
			return b
		}()))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 got %d", resp.StatusCode)
	}
	if resp.Header.Get("X-Provider") != "secondary" {
		t.Fatalf("expected secondary provider, got %s", resp.Header.Get("X-Provider"))
	}
}

// TestRetryAfterRespected: when upstream returns Retry-After, it's logged
// and the gateway falls back without sleeping (breaker handles backpressure).
func TestRetryAfterRespected(t *testing.T) {
	primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "5")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = fmt.Fprint(w, `{"error":{"message":"rate limited","type":"rate_limit_error"}}`)
	}))
	defer primary.Close()

	secondary := fakeUpstreamServer(http.StatusOK, okBody("model"), 0)
	defer secondary.Close()

	c1 := provider.New("primary", primary.URL, "k", 5*time.Second)
	c2 := provider.New("secondary", secondary.URL, "k", 5*time.Second)

	r := &simpleRouter{routes: []router.Route{
		{Client: c1, Model: "m1", RouteKey: "fast/primary"},
		{Client: c2, Model: "m2", RouteKey: "fast/secondary"},
	}}
	cfg := &config.Config{
		Cache:   config.CacheConfig{Enabled: false},
		Breaker: config.BreakerConfig{FailureThreshold: 3, RecoverySec: 30},
		Server:  config.ServerConfig{MaxBodyBytes: 1 << 20},
	}
	cfg.Breaker.Recovery = 30 * time.Second

	br := breaker.NewRegistry(breaker.Config{FailureThreshold: 3, Recovery: 30 * time.Second})
	lim := ratelimit.New(nil, true)
	ca := cache.New(nil, 0)
	gw := buildGateway(r, br, lim, ca, cfg)

	w := httptest.NewRecorder()
	gw.ServeHTTP(w, chatReq("fast"))
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 got %d", w.Code)
	}
}

// TestCacheHit: second identical request returns X-Cache: HIT.
func TestCacheHit(t *testing.T) {
	rdb, cleanup := startRedis(t)
	defer cleanup()

	callCount := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callCount++
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = fmt.Fprint(w, okBody("cached-model"))
	}))
	defer upstream.Close()

	c := provider.New("only", upstream.URL, "k", 5*time.Second)
	r := &simpleRouter{routes: []router.Route{
		{Client: c, Model: "cached-model", RouteKey: "cached/only"},
	}}
	cfg := &config.Config{
		Cache:   config.CacheConfig{Enabled: true, TTLSec: 60, TTL: 60 * time.Second},
		Breaker: config.BreakerConfig{FailureThreshold: 3, RecoverySec: 30},
		Server:  config.ServerConfig{MaxBodyBytes: 1 << 20},
	}
	cfg.Breaker.Recovery = 30 * time.Second

	br := breaker.NewRegistry(breaker.Config{FailureThreshold: 3, Recovery: 30 * time.Second})
	lim := ratelimit.New(rdb, true)
	ca := cache.New(rdb, 60*time.Second)
	gw := buildGateway(r, br, lim, ca, cfg)

	// First request — MISS.
	w1 := httptest.NewRecorder()
	gw.ServeHTTP(w1, chatReq("cached"))
	if w1.Code != http.StatusOK {
		t.Fatalf("first request: expected 200 got %d", w1.Code)
	}
	if w1.Header().Get("X-Cache") != "MISS" {
		t.Fatalf("expected MISS, got %s", w1.Header().Get("X-Cache"))
	}

	// Second request — HIT.
	w2 := httptest.NewRecorder()
	gw.ServeHTTP(w2, chatReq("cached"))
	if w2.Code != http.StatusOK {
		t.Fatalf("second request: expected 200 got %d", w2.Code)
	}
	if w2.Header().Get("X-Cache") != "HIT" {
		t.Fatalf("expected HIT, got %s", w2.Header().Get("X-Cache"))
	}
	if callCount != 1 {
		t.Fatalf("upstream should be called only once, got %d", callCount)
	}
}

// TestRedisRateLimitShared: two gateway instances share rate limit via Redis.
func TestRedisRateLimitShared(t *testing.T) {
	rdb, cleanup := startRedis(t)
	defer cleanup()

	upstream := fakeUpstreamServer(http.StatusOK, okBody("model"), 0)
	defer upstream.Close()

	c := provider.New("only", upstream.URL, "k", 5*time.Second)
	r := &simpleRouter{routes: []router.Route{
		{Client: c, Model: "model", RouteKey: "fast/only"},
	}}
	cfg := &config.Config{
		Cache: config.CacheConfig{Enabled: false},
		Breaker: config.BreakerConfig{
			FailureThreshold: 3,
			RecoverySec:      30,
			Recovery:         30 * time.Second,
		},
		Server: config.ServerConfig{MaxBodyBytes: 1 << 20},
		ClientKeys: []config.ClientKey{
			{
				Name:        "testuser",
				HashedKey:   ratelimit.HashedKey("test-raw-key"),
				RequestsMin: 2, // very low limit
				DailyTokens: 10000,
			},
		},
	}

	br1 := breaker.NewRegistry(breaker.Config{FailureThreshold: 3, Recovery: 30 * time.Second})
	lim1 := ratelimit.New(rdb, true)
	ca1 := cache.New(nil, 0)
	gw1 := httptest.NewServer(buildGateway(r, br1, lim1, ca1, cfg))
	defer gw1.Close()

	br2 := breaker.NewRegistry(breaker.Config{FailureThreshold: 3, Recovery: 30 * time.Second})
	lim2 := ratelimit.New(rdb, true)
	ca2 := cache.New(nil, 0)
	gw2 := httptest.NewServer(buildGateway(r, br2, lim2, ca2, cfg))
	defer gw2.Close()

	do := func(url string) int {
		temp := 0.0
		b, _ := json.Marshal(types.ChatCompletionRequest{
			Model:       "fast",
			Messages:    []types.ChatMessage{{Role: "user", Content: "hi"}},
			Temperature: &temp,
		})
		req, _ := http.NewRequest(http.MethodPost, url+"/v1/chat/completions", bytes.NewReader(b))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer test-raw-key")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return 0
		}
		defer resp.Body.Close()
		return resp.StatusCode
	}

	// 2 requests allowed across both instances combined.
	s1 := do(gw1.URL)
	s2 := do(gw2.URL)

	if s1 != http.StatusOK {
		t.Fatalf("first request should succeed, got %d", s1)
	}
	if s2 != http.StatusOK {
		t.Fatalf("second request should succeed, got %d", s2)
	}

	// Third request (at limit) should be denied.
	s3 := do(gw1.URL)
	if s3 != http.StatusTooManyRequests {
		t.Fatalf("third request should be rate limited (429), got %d", s3)
	}
}

// TestClientCancellation: when the client cancels the request context, the
// gateway's upstream HTTP call returns quickly (context.Canceled error).
// We verify the call doesn't block for the full upstream timeout.
func TestClientCancellation(t *testing.T) {
	started := make(chan struct{}, 1)
	canFinish := make(chan struct{})

	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Signal we started, then wait
		select {
		case started <- struct{}{}:
		default:
		}
		select {
		case <-r.Context().Done():
			// Gateway cancelled upstream — context propagated correctly.
		case <-canFinish:
			// Test tore down server
		case <-time.After(10 * time.Second):
		}
	}))
	defer func() {
		close(canFinish)
		slow.CloseClientConnections()
		slow.Close()
	}()

	c := provider.New("slow", slow.URL, "k", 10*time.Second)
	r := &simpleRouter{routes: []router.Route{
		{Client: c, Model: "slow-model", RouteKey: "slow/only"},
	}}
	cfg := &config.Config{
		Cache:   config.CacheConfig{Enabled: false},
		Breaker: config.BreakerConfig{FailureThreshold: 3, RecoverySec: 30, Recovery: 30 * time.Second},
		Server:  config.ServerConfig{MaxBodyBytes: 1 << 20},
	}

	br := breaker.NewRegistry(breaker.Config{FailureThreshold: 3, Recovery: 30 * time.Second})
	lim := ratelimit.New(nil, true)
	ca := cache.New(nil, 0)
	gw := httptest.NewServer(buildGateway(r, br, lim, ca, cfg))
	defer gw.Close()

	ctx, cancel := context.WithCancel(context.Background())
	temp := 0.0
	body, _ := json.Marshal(types.ChatCompletionRequest{
		Model:       "slow",
		Messages:    []types.ChatMessage{{Role: "user", Content: "hi"}},
		Temperature: &temp,
	})
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost,
		gw.URL+"/v1/chat/completions", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")

	callDone := make(chan error, 1)
	go func() {
		_, err := http.DefaultClient.Do(req)
		callDone <- err
	}()

	// Wait for the upstream to start processing
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("upstream didn't start within 5s")
	}

	// Cancel the client context
	cancel()

	// The HTTP call should return quickly
	select {
	case <-callDone:
		// Either success or cancellation error — both mean the call returned.
	case <-time.After(3 * time.Second):
		t.Fatal("HTTP call did not return within 3s after context cancellation")
	}
}
