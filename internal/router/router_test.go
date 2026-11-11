package router

import (
	"testing"
	"time"

	"github.com/mathuryashash/llm-gateway/internal/config"
	"github.com/mathuryashash/llm-gateway/internal/provider"
)

func TestRouterResolve(t *testing.T) {
	cfg := &config.Config{
		Routes: []config.RouteConfig{
			{Alias: "fast", Providers: []string{"groq"}, Models: []string{"llama-3.1-8b-instant"}},
		},
	}
	clients := map[string]*provider.Client{
		"groq": provider.New("groq", "https://api.groq.com/openai/v1", "fake", 30*time.Second),
	}
	r, err := New(cfg, clients)
	if err != nil {
		t.Fatal(err)
	}
	routes, err := r.Resolve("fast")
	if err != nil {
		t.Fatal(err)
	}
	if len(routes) != 1 {
		t.Fatalf("expected 1 route got %d", len(routes))
	}
	if routes[0].Model != "llama-3.1-8b-instant" {
		t.Fatalf("unexpected model %s", routes[0].Model)
	}
}

func TestRouterUnknownAlias(t *testing.T) {
	cfg := &config.Config{
		Routes: []config.RouteConfig{
			{Alias: "fast", Providers: []string{"groq"}, Models: []string{"llama-3.1-8b-instant"}},
		},
	}
	clients := map[string]*provider.Client{
		"groq": provider.New("groq", "https://api.groq.com/openai/v1", "fake", 30*time.Second),
	}
	r, err := New(cfg, clients)
	if err != nil {
		t.Fatal(err)
	}
	_, err = r.Resolve("nonexistent")
	if err == nil {
		t.Fatal("expected error for unknown alias")
	}
}

func TestRouterUnknownProvider(t *testing.T) {
	cfg := &config.Config{
		Routes: []config.RouteConfig{
			{Alias: "fast", Providers: []string{"unknown"}, Models: []string{"some-model"}},
		},
	}
	_, err := New(cfg, map[string]*provider.Client{})
	if err == nil {
		t.Fatal("expected error for unknown provider")
	}
}
