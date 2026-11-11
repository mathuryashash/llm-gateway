// Package provider implements HTTP clients for OpenAI-compatible upstream APIs.
// All four supported providers (Groq, Gemini via OpenAI compat, Nebius, NVIDIA NIM)
// use the same OpenAI /v1/chat/completions interface, so a single client suffices.
package provider

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/yashash-pushkar/llm-gateway/internal/types"
)

// Client is an HTTP client for one provider.
type Client struct {
	name    string
	baseURL string
	apiKey  string
	http    *http.Client
}

// New creates a Client.
func New(name, baseURL, apiKey string, timeout time.Duration) *Client {
	return &Client{
		name:    name,
		baseURL: strings.TrimRight(baseURL, "/"),
		apiKey:  apiKey,
		http: &http.Client{
			Timeout: timeout,
		},
	}
}

// Name returns the provider name.
func (c *Client) Name() string { return c.name }

// ChatResult is returned by Chat/ChatStream.
type ChatResult struct {
	Response   *types.ChatCompletionResponse
	StatusCode int
	RetryAfter int    // seconds; >0 when provider sent Retry-After
	Body       []byte // raw response body for caching
}

// Chat performs a non-streaming chat completion and returns the parsed response.
func (c *Client) Chat(ctx context.Context, model string, req *types.ChatCompletionRequest) (*ChatResult, error) {
	// Override the model field with the provider-specific name.
	reqCopy := *req
	reqCopy.Model = model
	reqCopy.Stream = false

	body, err := json.Marshal(reqCopy)
	if err != nil {
		return nil, fmt.Errorf("provider %s: marshal request: %w", c.name, err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.baseURL+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("provider %s: build request: %w", c.name, err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+c.apiKey)

	resp, err := c.http.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("provider %s: http: %w", c.name, err)
	}
	defer resp.Body.Close()

	rawBody, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20)) // 4 MB limit
	if err != nil {
		return nil, fmt.Errorf("provider %s: read body: %w", c.name, err)
	}

	result := &ChatResult{
		StatusCode: resp.StatusCode,
		Body:       rawBody,
		RetryAfter: parseRetryAfter(resp.Header.Get("Retry-After")),
	}

	if resp.StatusCode != http.StatusOK {
		return result, nil // caller inspects StatusCode
	}

	var chatResp types.ChatCompletionResponse
	if err := json.Unmarshal(rawBody, &chatResp); err != nil {
		return nil, fmt.Errorf("provider %s: decode response: %w", c.name, err)
	}
	result.Response = &chatResp
	return result, nil
}

// StreamCallback is called for each SSE line received from the upstream.
// The function should write the line to the client response writer.
type StreamCallback func(line string) error

// ChatStream performs a streaming chat completion and calls cb for each SSE
// data line. It returns the HTTP status code and any error.
//
// Safety: if the upstream returns a non-2xx status before sending any SSE
// bytes, we return early and the caller may try the next provider.
// Once SSE bytes have been forwarded to the client, we MUST NOT retry on
// a different provider — the client already received partial data.
func (c *Client) ChatStream(ctx context.Context, model string, req *types.ChatCompletionRequest, cb StreamCallback) (int, error) {
	reqCopy := *req
	reqCopy.Model = model
	reqCopy.Stream = true

	body, err := json.Marshal(reqCopy)
	if err != nil {
		return 0, fmt.Errorf("provider %s: marshal: %w", c.name, err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.baseURL+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return 0, fmt.Errorf("provider %s: build: %w", c.name, err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+c.apiKey)
	httpReq.Header.Set("Accept", "text/event-stream")

	resp, err := c.http.Do(httpReq)
	if err != nil {
		return 0, fmt.Errorf("provider %s: http: %w", c.name, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return resp.StatusCode, nil
	}

	scanner := bufio.NewScanner(resp.Body)
	for scanner.Scan() {
		line := scanner.Text()
		if err := cb(line); err != nil {
			return resp.StatusCode, fmt.Errorf("provider %s: stream write: %w", c.name, err)
		}
	}
	if err := scanner.Err(); err != nil {
		if ctx.Err() != nil {
			return resp.StatusCode, ctx.Err() // client cancelled
		}
		return resp.StatusCode, fmt.Errorf("provider %s: stream scan: %w", c.name, err)
	}
	return resp.StatusCode, nil
}

// parseRetryAfter parses the Retry-After header value (seconds or HTTP-date).
// Returns 0 if absent or unparseable.
func parseRetryAfter(h string) int {
	if h == "" {
		return 0
	}
	if n, err := strconv.Atoi(h); err == nil {
		return n
	}
	// Try HTTP-date format.
	t, err := http.ParseTime(h)
	if err != nil {
		return 0
	}
	secs := int(time.Until(t).Seconds())
	if secs < 0 {
		return 0
	}
	return secs
}
