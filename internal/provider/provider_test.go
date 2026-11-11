package provider

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/mathuryashash/llm-gateway/internal/types"
)

func fakeServer(t *testing.T, statusCode int, body string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(statusCode)
		_, _ = w.Write([]byte(body))
	}))
}

func TestChatSuccess(t *testing.T) {
	resp := types.ChatCompletionResponse{
		ID:     "test-id",
		Object: "chat.completion",
		Model:  "test-model",
		Choices: []types.ChatCompletionChoice{
			{Index: 0, Message: types.ChatMessage{Role: "assistant", Content: "hello"}, FinishReason: "stop"},
		},
	}
	body, _ := json.Marshal(resp)

	srv := fakeServer(t, http.StatusOK, string(body))
	defer srv.Close()

	c := New("test", srv.URL, "key", 5*time.Second)
	req := &types.ChatCompletionRequest{
		Model:    "test-model",
		Messages: []types.ChatMessage{{Role: "user", Content: "hi"}},
	}

	result, err := c.Chat(context.Background(), "test-model", req)
	if err != nil {
		t.Fatal(err)
	}
	if result.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 got %d", result.StatusCode)
	}
	if result.Response == nil {
		t.Fatal("response should not be nil")
	}
	if result.Response.ID != "test-id" {
		t.Fatalf("unexpected ID %s", result.Response.ID)
	}
}

func TestChatUpstream429(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "10")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"error":{"message":"rate limited"}}`))
	}))
	defer srv.Close()

	c := New("test", srv.URL, "key", 5*time.Second)
	req := &types.ChatCompletionRequest{Model: "m", Messages: []types.ChatMessage{{Role: "user", Content: "hi"}}}

	result, err := c.Chat(context.Background(), "m", req)
	if err != nil {
		t.Fatal(err)
	}
	if result.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("expected 429 got %d", result.StatusCode)
	}
	if result.RetryAfter != 10 {
		t.Fatalf("expected RetryAfter=10 got %d", result.RetryAfter)
	}
}

func TestChatContextCancellation(t *testing.T) {
	started := make(chan struct{}, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		started <- struct{}{}
		select {
		case <-r.Context().Done():
		case <-time.After(500 * time.Millisecond):
		}
	}))
	defer func() {
		srv.CloseClientConnections()
		srv.Close()
	}()

	ctx, cancel := context.WithCancel(context.Background())
	c := New("test", srv.URL, "key", 5*time.Second)
	req := &types.ChatCompletionRequest{Model: "m", Messages: []types.ChatMessage{{Role: "user", Content: "hi"}}}

	go func() {
		<-started
		cancel()
	}()

	_, err := c.Chat(ctx, "m", req)
	if err == nil {
		t.Fatal("expected error on cancelled context")
	}
}

func TestParseRetryAfterSeconds(t *testing.T) {
	if parseRetryAfter("30") != 30 {
		t.Fatal("expected 30")
	}
}

func TestParseRetryAfterEmpty(t *testing.T) {
	if parseRetryAfter("") != 0 {
		t.Fatal("expected 0 for empty")
	}
}

func TestChatStream(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher := w.(http.Flusher)
		for _, chunk := range []string{
			`data: {"choices":[{"delta":{"content":"hello"}}]}`,
			`data: [DONE]`,
		} {
			_, _ = w.Write([]byte(chunk + "\n"))
			flusher.Flush()
		}
	}))
	defer srv.Close()

	c := New("test", srv.URL, "key", 5*time.Second)
	req := &types.ChatCompletionRequest{Model: "m", Messages: []types.ChatMessage{{Role: "user", Content: "hi"}}, Stream: true}

	var lines []string
	status, err := c.ChatStream(context.Background(), "m", req, func(line string) error {
		lines = append(lines, line)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if status != http.StatusOK {
		t.Fatalf("expected 200 got %d", status)
	}
	combined := strings.Join(lines, " ")
	if !strings.Contains(combined, "[DONE]") {
		t.Fatalf("expected [DONE] in stream, got: %v", lines)
	}
}
