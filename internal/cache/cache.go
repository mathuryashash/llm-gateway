// Package cache implements a Redis-backed response cache for deterministic
// LLM requests (temperature == 0 or X-Cache-Control: force header).
//
// Cache key = SHA-256 of the canonically serialised request fields that
// affect the output: model, messages, temperature, top_p, max_tokens.
// Streamed responses are NOT cached (safely buffering an SSE stream and
// then re-streaming it adds complexity and latency; the trade-off is
// documented in DESIGN.md).
package cache

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"
)

const keyPrefix = "cache:resp:"

// CachedResponse is what we serialise into Redis.
type CachedResponse struct {
	Body       []byte `json:"body"` // raw JSON response bytes
	StatusCode int    `json:"status_code"`
	Provider   string `json:"provider"`
	CachedAt   int64  `json:"cached_at"` // unix seconds
}

// Cache wraps a Redis client for response caching.
type Cache struct {
	rdb *redis.Client
	ttl time.Duration
}

// New returns a Cache. If rdb is nil all calls are no-ops.
func New(rdb *redis.Client, ttl time.Duration) *Cache {
	return &Cache{rdb: rdb, ttl: ttl}
}

// RequestKey is the subset of request fields that determine the response.
// It must be stable across serialisations (sorted keys in JSON is handled
// by using a struct, not a map).
type RequestKey struct {
	Model       string   `json:"model"`
	Messages    []msgKey `json:"messages"`
	Temperature float64  `json:"temperature"`
	TopP        float64  `json:"top_p"`
	MaxTokens   int      `json:"max_tokens"`
}

// MsgKey is an exported type alias for cache key construction.
type MsgKey = msgKey

type msgKey struct {
	Role    string `json:"role"`
	Content string `json:"content"` // string only; ignore multi-part for caching
}

// Key computes the cache key for a request.
func Key(rk RequestKey) string {
	data, _ := json.Marshal(rk)
	h := sha256.Sum256(data)
	return keyPrefix + hex.EncodeToString(h[:])
}

// Get returns a cached response, or (nil, nil) on miss.
func (c *Cache) Get(ctx context.Context, key string) (*CachedResponse, error) {
	if c.rdb == nil {
		return nil, nil
	}
	data, err := c.rdb.Get(ctx, key).Bytes()
	if err == redis.Nil {
		return nil, nil // miss
	}
	if err != nil {
		return nil, fmt.Errorf("cache get %s: %w", key, err)
	}
	var resp CachedResponse
	if err := json.Unmarshal(data, &resp); err != nil {
		// Corrupt entry — treat as miss.
		_ = c.rdb.Del(ctx, key)
		return nil, nil
	}
	return &resp, nil
}

// Set stores a response in the cache.
func (c *Cache) Set(ctx context.Context, key string, resp *CachedResponse) error {
	if c.rdb == nil {
		return nil
	}
	resp.CachedAt = time.Now().Unix()
	data, err := json.Marshal(resp)
	if err != nil {
		return fmt.Errorf("cache marshal: %w", err)
	}
	if err := c.rdb.Set(ctx, key, data, c.ttl).Err(); err != nil {
		return fmt.Errorf("cache set %s: %w", key, err)
	}
	return nil
}

// IsDeterministic returns true when the request should be cached.
// Only cache when temperature is explicitly 0 (deterministic output) and
// stream is false. A nil temperature means "not set by client" — we do not
// cache because the provider may default to a non-zero temperature.
func IsDeterministic(temperature *float64, stream bool) bool {
	if stream {
		return false // never cache streams
	}
	if temperature == nil {
		return false // unknown temperature — do not cache
	}
	return *temperature == 0
}
