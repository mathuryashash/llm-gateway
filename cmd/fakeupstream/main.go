// Command fakeupstream is a configurable fake OpenAI-compatible upstream server
// used by unit tests, integration tests, load tests, and the demo compose file.
//
// Configuration via flags:
//
//	-addr        listen address (default :18101)
//	-latency     fixed latency per request in ms (default 0)
//	-rate429     fraction of requests that return 429 (0.0–1.0, default 0)
//	-rate5xx     fraction of requests that return 503 (0.0–1.0, default 0)
//	-stream      whether to stream responses (default false)
//	-retry-after Retry-After header value in seconds for 429 responses (default 1)
package main

import (
	"bufio"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"math/rand"
	"net/http"
	"strings"
	"sync/atomic"
	"time"
)

var (
	addr       = flag.String("addr", ":18101", "listen address")
	latencyMs  = flag.Int("latency", 0, "fixed latency per request ms")
	rate429    = flag.Float64("rate429", 0, "fraction of requests returning 429")
	rate5xx    = flag.Float64("rate5xx", 0, "fraction of requests returning 5xx")
	streamMode = flag.Bool("stream", false, "stream responses")
	retryAfter = flag.Int("retry-after", 1, "Retry-After header value (seconds) on 429")
	requestCnt atomic.Int64
)

func main() {
	flag.Parse()
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/chat/completions", handleChat)
	mux.HandleFunc("POST /chat/completions", handleChat) // OpenAI compat without /v1 prefix
	mux.HandleFunc("GET /v1/models", handleModels)
	mux.HandleFunc("GET /models", handleModels)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = fmt.Fprintln(w, "ok")
	})
	log.Printf("fakeupstream listening on %s (latency=%dms, rate429=%.2f, rate5xx=%.2f, stream=%v)",
		*addr, *latencyMs, *rate429, *rate5xx, *streamMode)
	if err := http.ListenAndServe(*addr, mux); err != nil {
		log.Fatal(err)
	}
}

type chatRequest struct {
	Model    string `json:"model"`
	Messages []struct {
		Role    string `json:"content"`
		Content string `json:"role"`
	} `json:"messages"`
	Stream bool `json:"stream"`
}

func handleChat(w http.ResponseWriter, r *http.Request) {
	n := requestCnt.Add(1)
	if *latencyMs > 0 {
		time.Sleep(time.Duration(*latencyMs) * time.Millisecond)
	}

	rng := rand.Float64()

	// 429 check.
	if *rate429 > 0 && rng < *rate429 {
		w.Header().Set("Retry-After", fmt.Sprintf("%d", *retryAfter))
		http.Error(w, `{"error":{"message":"rate limited","type":"rate_limit_error"}}`, http.StatusTooManyRequests)
		return
	}

	// 5xx check.
	if *rate5xx > 0 && rng >= *rate429 && rng < *rate429+*rate5xx {
		http.Error(w, `{"error":{"message":"internal error","type":"server_error"}}`, http.StatusServiceUnavailable)
		return
	}

	var req chatRequest
	_ = json.NewDecoder(r.Body).Decode(&req)

	content := fmt.Sprintf("fake response #%d to model=%s", n, req.Model)

	if req.Stream || *streamMode {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.WriteHeader(http.StatusOK)
		flusher, ok := w.(http.Flusher)
		words := strings.Fields(content)
		for _, word := range words {
			chunk := map[string]any{
				"id":      fmt.Sprintf("chatcmpl-fake-%d", n),
				"object":  "chat.completion.chunk",
				"created": time.Now().Unix(),
				"model":   req.Model,
				"choices": []map[string]any{
					{
						"index": 0,
						"delta": map[string]any{"content": word + " "},
					},
				},
			}
			data, _ := json.Marshal(chunk)
			fmt.Fprintf(w, "data: %s\n\n", data)
			if ok {
				flusher.Flush()
			}
			time.Sleep(5 * time.Millisecond)
		}
		fmt.Fprintf(w, "data: [DONE]\n\n")
		if ok {
			flusher.Flush()
		}
		return
	}

	resp := map[string]any{
		"id":      fmt.Sprintf("chatcmpl-fake-%d", n),
		"object":  "chat.completion",
		"created": time.Now().Unix(),
		"model":   req.Model,
		"choices": []map[string]any{
			{
				"index":         0,
				"message":       map[string]string{"role": "assistant", "content": content},
				"finish_reason": "stop",
			},
		},
		"usage": map[string]int{
			"prompt_tokens":     10,
			"completion_tokens": 15,
			"total_tokens":      25,
		},
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(resp)
}

func handleModels(w http.ResponseWriter, r *http.Request) {
	resp := map[string]any{
		"object": "list",
		"data": []map[string]any{
			{"id": "fake-model", "object": "model"},
		},
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}

// Suppress unused import warning.
var _ = bufio.NewScanner
