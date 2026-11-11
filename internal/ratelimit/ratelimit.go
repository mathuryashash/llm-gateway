// Package ratelimit implements per-client token-bucket rate limiting backed by
// Redis Lua scripts so multiple gateway replicas share a single counter.
//
// Two limits are enforced per client:
//  1. requests/minute — classic token bucket (capacity = limit, refill = limit/60s).
//  2. daily token budget — a simple counter that resets at UTC midnight.
//
// When Redis is unavailable the limiter falls back to in-memory buckets.
// Whether to fail-open (allow) or fail-closed (deny) on Redis errors is
// configurable; default is fail-open (see DESIGN.md for the reasoning).
package ratelimit

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"math"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"
)

// Decision is the result of an Allow call.
type Decision int

const (
	Allowed Decision = iota
	DeniedRPM   // rate limit: requests per minute
	DeniedDaily // rate limit: daily token budget
)

func (d Decision) String() string {
	switch d {
	case Allowed:
		return "allowed"
	case DeniedRPM:
		return "denied_rpm"
	case DeniedDaily:
		return "denied_daily"
	default:
		return "unknown"
	}
}

// tokenBucketScript is an atomic Lua script run server-side.
// Keys: [bucket_key]
// Args: [capacity, refill_tokens_per_sec * 1000 (integer milli), now_unix_ms]
// Returns: 1 if allowed, 0 if denied; also updates bucket state.
const tokenBucketScript = `
local key      = KEYS[1]
local cap      = tonumber(ARGV[1])
local refill   = tonumber(ARGV[2]) -- tokens per ms * 1000
local now      = tonumber(ARGV[3]) -- ms

local data = redis.call('HMGET', key, 'tokens', 'last')
local tokens = tonumber(data[1]) or cap
local last   = tonumber(data[2]) or now

local elapsed = math.max(0, now - last)
local add     = math.floor(elapsed * refill / 1000)
tokens = math.min(cap, tokens + add)

if tokens >= 1 then
  tokens = tokens - 1
  redis.call('HMSET', key, 'tokens', tokens, 'last', now)
  redis.call('PEXPIRE', key, 120000)
  return 1
else
  redis.call('HMSET', key, 'tokens', tokens, 'last', now)
  redis.call('PEXPIRE', key, 120000)
  return 0
end
`

// dailyBudgetScript atomically increments and checks the daily token counter.
// Keys: [budget_key]
// Args: [max_tokens, tokens_used, ttl_seconds_to_midnight]
// Returns: 1 if under budget, 0 if over.
const dailyBudgetScript = `
local key     = KEYS[1]
local max     = tonumber(ARGV[1])
local used    = tonumber(ARGV[2])
local ttl     = tonumber(ARGV[3])

local cur = tonumber(redis.call('GET', key) or 0)
if cur + used > max then
  return 0
end
redis.call('INCRBY', key, used)
redis.call('EXPIRE', key, ttl)
return 1
`

// Limiter checks both request-rate and daily budget limits for a client key.
type Limiter struct {
	rdb      *redis.Client
	failOpen bool

	// fallback in-memory buckets used when Redis is unavailable.
	mu      sync.Mutex
	buckets map[string]*memBucket
}

// memBucket is a simple in-memory token bucket for the Redis-down fallback.
type memBucket struct {
	tokens   float64
	capacity float64
	refill   float64 // tokens per nanosecond
	last     time.Time
}

func (m *memBucket) allow() bool {
	now := time.Now()
	elapsed := now.Sub(m.last).Seconds()
	m.tokens = math.Min(m.capacity, m.tokens+elapsed*m.refill)
	m.last = now
	if m.tokens >= 1 {
		m.tokens--
		return true
	}
	return false
}

// New creates a Limiter.
func New(rdb *redis.Client, failOpen bool) *Limiter {
	return &Limiter{
		rdb:      rdb,
		failOpen: failOpen,
		buckets:  make(map[string]*memBucket),
	}
}

// HashedKey computes the SHA-256 hex of a raw client API key.
// Gateway config stores hashed keys; incoming requests carry raw keys.
func HashedKey(raw string) string {
	h := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(h[:])
}

// Allow checks whether clientHash (hashed key) may make a request.
//   - requestsPerMin: bucket capacity and per-minute refill rate.
//   - dailyTokenBudget: maximum tokens consumed per UTC day.
//   - tokensUsed: tokens this request would consume (0 for pre-check).
func (l *Limiter) Allow(ctx context.Context, clientHash string, requestsPerMin, dailyTokenBudget, tokensUsed int) (Decision, error) {
	rpmKey := fmt.Sprintf("rl:rpm:%s", clientHash)
	dayKey := fmt.Sprintf("rl:day:%s:%s", clientHash, utcDay())

	// --- Redis path ---
	if l.rdb != nil {
		nowMs := time.Now().UnixMilli()
		// refill in milli-tokens per ms = requestsPerMin * 1000 / 60000
		refill := requestsPerMin * 1000 / 60 // integer milli-tokens per second

		rpmOK, err := redis.NewScript(tokenBucketScript).Run(ctx, l.rdb,
			[]string{rpmKey},
			requestsPerMin, refill, nowMs,
		).Int()
		if err != nil && err != redis.Nil {
			// Redis unavailable — fall back
			return l.fallback(clientHash, requestsPerMin), nil
		}
		if rpmOK == 0 {
			return DeniedRPM, nil
		}

		if dailyTokenBudget > 0 && tokensUsed > 0 {
			ttl := secondsToMidnight()
			dayOK, err := redis.NewScript(dailyBudgetScript).Run(ctx, l.rdb,
				[]string{dayKey},
				dailyTokenBudget, tokensUsed, ttl,
			).Int()
			if err != nil && err != redis.Nil {
				if !l.failOpen {
					return DeniedDaily, nil
				}
				// fail-open: allow
			} else if dayOK == 0 {
				return DeniedDaily, nil
			}
		}
		return Allowed, nil
	}

	// --- In-memory fallback ---
	return l.fallback(clientHash, requestsPerMin), nil
}

func (l *Limiter) fallback(clientHash string, requestsPerMin int) Decision {
	l.mu.Lock()
	defer l.mu.Unlock()
	b, ok := l.buckets[clientHash]
	if !ok {
		b = &memBucket{
			tokens:   float64(requestsPerMin),
			capacity: float64(requestsPerMin),
			refill:   float64(requestsPerMin) / 60.0, // per second
			last:     time.Now(),
		}
		l.buckets[clientHash] = b
	}
	if b.allow() {
		return Allowed
	}
	return DeniedRPM
}

// utcDay returns a string like "2024-01-15" for the current UTC date.
func utcDay() string {
	return time.Now().UTC().Format("2006-01-02")
}

// secondsToMidnight returns seconds until UTC midnight.
func secondsToMidnight() int {
	now := time.Now().UTC()
	midnight := time.Date(now.Year(), now.Month(), now.Day()+1, 0, 0, 0, 0, time.UTC)
	return int(time.Until(midnight).Seconds())
}
