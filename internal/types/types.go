// Package types defines the shared request/response types for the LLM gateway.
// We use the OpenAI-compatible format so any existing OpenAI SDK works unchanged.
package types

// ChatMessage is a single turn in a conversation.
type ChatMessage struct {
	Role    string `json:"role"`
	Content any    `json:"content"` // string or array of content parts
}

// ChatCompletionRequest mirrors the OpenAI /v1/chat/completions request body.
// Fields not understood by a provider are forwarded as-is.
type ChatCompletionRequest struct {
	Model       string        `json:"model"`
	Messages    []ChatMessage `json:"messages"`
	MaxTokens   int           `json:"max_tokens,omitempty"`
	Temperature *float64      `json:"temperature,omitempty"`
	TopP        *float64      `json:"top_p,omitempty"`
	Stream      bool          `json:"stream,omitempty"`
	User        string        `json:"user,omitempty"`
	// Extra fields (tool_calls, functions, etc.) passed through raw.
	Extra map[string]any `json:"-"`
}

// ChatCompletionChoice is one completion choice.
type ChatCompletionChoice struct {
	Index        int         `json:"index"`
	Message      ChatMessage `json:"message"`
	FinishReason string      `json:"finish_reason"`
}

// UsageInfo is token accounting from the upstream provider.
type UsageInfo struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

// ChatCompletionResponse is the upstream (and gateway) response.
type ChatCompletionResponse struct {
	ID      string                 `json:"id"`
	Object  string                 `json:"object"`
	Created int64                  `json:"created"`
	Model   string                 `json:"model"`
	Choices []ChatCompletionChoice `json:"choices"`
	Usage   UsageInfo              `json:"usage"`
}

// ErrorResponse is the OpenAI-compatible error envelope.
type ErrorResponse struct {
	Error ErrorDetail `json:"error"`
}

// ErrorDetail holds the error message, type, and optional code.
type ErrorDetail struct {
	Message string `json:"message"`
	Type    string `json:"type"`
	Code    string `json:"code,omitempty"`
}

// UpstreamResult carries either a successful response or an error from one
// provider attempt, plus metadata used for routing decisions.
type UpstreamResult struct {
	Response     *ChatCompletionResponse
	StatusCode   int
	RetryAfter   int // seconds; >0 when provider sent Retry-After
	ProviderName string
	Latency      int64 // ms
}
