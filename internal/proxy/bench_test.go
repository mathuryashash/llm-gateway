package proxy

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/mathuryashash/llm-gateway/internal/breaker"
	"github.com/mathuryashash/llm-gateway/internal/cache"
	"github.com/mathuryashash/llm-gateway/internal/config"
	"github.com/mathuryashash/llm-gateway/internal/provider"
	"github.com/mathuryashash/llm-gateway/internal/ratelimit"
	"github.com/mathuryashash/llm-gateway/internal/router"
	"github.com/mathuryashash/llm-gateway/internal/types"
	"io"
	"log/slog"
	"os"
)

func BenchmarkChatHandler(b *testing.B) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		resp := types.ChatCompletionResponse{
			ID: "bench", Object: "chat.completion",
			Choices: []types.ChatCompletionChoice{
				{Index: 0, Message: types.ChatMessage{Role: "assistant", Content: "bench response"}, FinishReason: "stop"},
			},
		}
		json.NewEncoder(w).Encode(resp)
	}))
	defer upstream.Close()

	c := provider.New("bench", upstream.URL, "k", 5*time.Second)
	r := &mockRouter{routes: []router.Route{{Client: c, Model: "m", RouteKey: "bench/bench"}}}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	cfg := &config.Config{
		Cache:   config.CacheConfig{Enabled: false},
		Breaker: config.BreakerConfig{FailureThreshold: 3, RecoverySec: 30, Recovery: 30 * time.Second},
		Server:  config.ServerConfig{MaxBodyBytes: 1 << 20},
	}
	br := breaker.NewRegistry(breaker.Config{FailureThreshold: 3, Recovery: 30 * time.Second})
	lim := ratelimit.New(nil, true)
	ca := cache.New(nil, 0)
	h := New(r, br, lim, ca, logger, cfg)

	temp := 1.0
	body := types.ChatCompletionRequest{
		Model:       "bench",
		Messages:    []types.ChatMessage{{Role: "user", Content: "hello"}},
		Temperature: &temp,
	}
	bodyBytes, _ := json.Marshal(body)

	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			w := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(bodyBytes))
			req.Header.Set("Content-Type", "application/json")
			h.ServeHTTP(w, req)
			if w.Code != http.StatusOK {
				b.Fatalf("unexpected status %d", w.Code)
			}
		}
	})
}

func BenchmarkBreakerAllow(b *testing.B) {
	br := breaker.New(3, time.Second)
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			if br.Allow() {
				br.RecordSuccess()
			}
		}
	})
}

func BenchmarkHashKey(b *testing.B) {
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		ratelimit.HashedKey("test-api-key-for-benchmarking")
	}
}

// suppress unused imports
var _ = os.Stderr
