# Design Notes: LLM Gateway

## Why this architecture

This gateway exists because LLM provider APIs are unreliable in ways that are
annoying to handle in every application: rate limits (429), occasional 5xx errors,
and latency spikes. The gateway centralises the handling of all three so
application code just calls one endpoint.

## Key decisions

### 1. Single OpenAI-compatible interface
Every supported provider (Groq, Gemini via OpenAI compat endpoint, Nebius, NVIDIA
NIM) accepts the same OpenAI `/v1/chat/completions` schema. We use one provider.go
HTTP client for all four instead of four separate adapters. When a provider's
endpoint differs we adjust only the base URL and model name in config, not the code.

**Trade-off**: if a provider adds features outside the OpenAI schema (custom fields,
different auth headers), we cannot expose them without code changes.

### 2. Circuit breaker per route key
Each `(model-alias, provider)` pair gets its own circuit breaker. Opening one
breaker does not block traffic to any other route. This is more granular than
a per-provider breaker: if provider A is rate-limited for model X but healthy
for model Y, model Y requests still flow through.

**States**: Closed → Open (after N consecutive failures) → HalfOpen (after recovery
window) → Closed/Open (probe result).

**We chose 3 consecutive failures** as the threshold (configurable). A single
network hiccup should not open the breaker; three in a row likely indicates a real
problem.

### 3. Rate limiting: Redis Lua, in-memory fallback
We use an atomic Lua script on the Redis server to implement the token bucket. This
avoids the race condition of a read-increment-write cycle across two gateway
replicas sharing one Redis.

**Fail-open vs fail-closed**: when Redis is unavailable the gateway falls back to
per-process in-memory buckets. Default is fail-open (allow traffic). This trades
strict enforcement for availability. If strict enforcement is required (e.g. to
protect against abuse), set `rate_limit.fail_open: false` — this blocks all rate-
limited clients when Redis is down. For a job-hunt agent (the motivating use case),
availability matters more than strict per-minute accounting.

### 4. Cache: deterministic requests only
We only cache when `temperature == 0` and `stream == false`. A non-zero temperature
produces non-deterministic outputs; caching them would return the same stale answer
to what the caller intended to be a varied request. Streams are not cached because
buffering the entire SSE stream before forwarding it adds unbounded latency and
memory pressure.

The cache key is SHA-256 of the canonically serialised request fields
(model, messages, temperature, top_p, max_tokens). Using a struct (not a map)
ensures stable JSON serialisation order.

### 5. Partial stream safety
Once SSE bytes have been forwarded to the client we cannot retry on a different
provider — the client has already received partial content. The proxy attempts the
next provider **only if the upstream returns a non-2xx status before sending any
SSE bytes** (`bytesSent == 0` gate). This is documented in proxy.go.

### 6. No global middleware for metrics
Prometheus metrics are recorded inside the proxy handler (not an HTTP middleware)
because they need context that only the proxy has: which provider actually handled
the request, whether a fallback occurred, the upstream latency per route key. A
generic middleware would only give end-to-end latency and status codes.

### 7. Distroless final image
The Dockerfile uses `gcr.io/distroless/static-debian12:nonroot` as the final layer.
There is no shell, no package manager, and the process runs as a non-root user. This
reduces the attack surface to just the gateway binary and its direct Go dependencies.

## Failure modes

| Failure | Behaviour |
|---------|-----------|
| All providers rate-limited | 502; client should back off |
| Redis down | In-memory fallback for rate limiting; cache disabled |
| Provider 5xx | Breaker counts, fallback to next route |
| Partial stream then upstream error | Error logged; client gets truncated stream (safe) |
| Config missing model | 404 with OpenAI-compatible error body |
| Client context cancelled | Upstream call cancelled via context propagation |
| Body > max_body_bytes | 413 before reading the body |

## How to scale further

1. **Horizontal scaling**: the gateway is stateless (state in Redis). Add replicas
   behind a load balancer. The Redis Lua scripts ensure consistent rate limiting.

2. **More providers**: add a new entry in `config.yaml` under `providers` with the
   base URL and key env var. No code change needed.

3. **Weighted routing**: the current route list is strict priority order. A weighted
   random selection would be easy to add: replace `[]Route` with `[]WeightedRoute`.

4. **Streaming cache**: buffering a full SSE response before forwarding and storing
   it is achievable but adds complexity. The trade-off is documented in the cache
   package — worth revisiting if the response sizes are known and bounded.

5. **Multi-modal**: the OpenAI schema supports image/audio content. The current
   `Content any` field passes them through; providers that support them will work.
   Cache key logic would need to handle binary content hashing.

## Alternatives considered

- **chi router**: we use the standard `net/http.ServeMux` (Go 1.22+) which now
  supports method-prefixed patterns (`POST /v1/chat/completions`). No external
  routing dependency needed.

- **Dedicated breaker library** (sony/gobreaker): we implemented our own because
  it's small (< 100 lines), easy to explain in an interview, and avoids a
  dependency. The state machine is exactly what the spec requires.

- **Redis streams vs Lua**: Redis MULTI/EXEC transactions would also work but Lua
  scripts are more efficient (single round trip, no watch/retry).
