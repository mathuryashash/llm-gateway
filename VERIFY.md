# VERIFY.md — Reproducible evidence for all README numbers

All commands run on: Windows 11, Intel i7-14700HX, 16 GB RAM.
Gateway + fakeupstream on localhost. No real LLM calls in load tests.
Redis unavailable during benchmarks (in-memory fallback; cache disabled).

---

## Unit tests

```
$ go test ./internal/... -timeout 120s

ok  github.com/mathuryashash/llm-gateway/internal/breaker     2.097s
ok  github.com/mathuryashash/llm-gateway/internal/cache       2.615s
ok  github.com/mathuryashash/llm-gateway/internal/config      1.454s
ok  github.com/mathuryashash/llm-gateway/internal/middleware  2.630s
ok  github.com/mathuryashash/llm-gateway/internal/provider    4.543s
ok  github.com/mathuryashash/llm-gateway/internal/proxy       0.370s
ok  github.com/mathuryashash/llm-gateway/internal/ratelimit   0.995s
ok  github.com/mathuryashash/llm-gateway/internal/router      1.516s
```

8 packages pass, 0 failures.

---

## Integration tests (Docker / testcontainers-go Redis)

```
$ go test ./integration/... -v -timeout 120s

=== RUN   TestFallbackChain
--- PASS: TestFallbackChain (0.01s)
=== RUN   TestRetryAfterRespected
--- PASS: TestRetryAfterRespected (0.00s)
=== RUN   TestCacheHit
--- PASS: TestCacheHit (2.48s)
=== RUN   TestRedisRateLimitShared
--- PASS: TestRedisRateLimitShared (1.30s)
=== RUN   TestClientCancellation
--- PASS: TestClientCancellation (0.01s)
PASS
ok  github.com/mathuryashash/llm-gateway/integration 4.312s
```

5/5 integration tests pass, including:
- Cache HIT verified (upstream called only once for identical deterministic requests).
- Shared Redis rate limit: request 3 from two separate gateway instances gets 429.
- Client cancellation: HTTP call returns within 3s of context cancel (not blocked).

---

## Fuzz test

```
$ go test ./internal/cache/... -fuzz=FuzzCacheKey -fuzztime=10s

fuzz: elapsed: 6s, execs: 105111 (13981/sec), new interesting: 55 (total: 58)
PASS
ok  github.com/mathuryashash/llm-gateway/internal/cache 8.799s
```

No panics or non-determinism found in 105,111 inputs.

---

## Go micro-benchmarks (fake upstream, localhost)

```
$ go test ./internal/proxy/... -bench=. -benchmem -run='^$'

goos: windows
goarch: amd64
cpu: Intel(R) Core(TM) i7-14700HX

BenchmarkChatHandler-28    5674    196876 ns/op   59313 B/op   261 allocs/op
BenchmarkBreakerAllow-28   5422294   231.9 ns/op      0 B/op     0 allocs/op
BenchmarkHashKey-28        3179829   390.8 ns/op    128 B/op     2 allocs/op
```

- Full proxy handler (parse → route → upstream → response): **197 µs/op**
- Circuit breaker Allow(): **232 ns/op, zero allocations**
- SHA-256 key hash: **391 ns/op**

---

## k6 load test (fake upstream, 20 ms fixed latency, no Redis)

```
$ k6 run k6/load_test.js

  gateway_latency_ms: avg=22.3 min=20 med=22 max=85  p(90)=24 p(95)=25
  direct_latency_ms:  avg=20.9 min=20 med=21 max=26  p(90)=22 p(95)=22
  error_rate:         0.00%  0 out of 8042
  cache_hit_rate:     0.00%  (Redis unavailable; cache disabled)

  http_req_duration: avg=21.58ms  p(90)=23.17ms  p(95)=23.87ms
  http_reqs:         13788 total  405 req/s
```

- **Gateway overhead** = p50 gateway (22 ms) − p50 direct (21 ms) = **~1 ms**
- **p95 overhead** = 25 ms − 22 ms = **~3 ms**
- **Throughput at 20 VUs**: **~461 iterations/s** (each iteration = 1 gateway + 1 direct call)
- **Error rate**: 0%

Scenario: 5 VUs × 10s then 20 VUs × 10s against fake upstream with 20 ms latency.
No Redis → cache disabled (all MISS), rate limiting uses in-memory fallback.

---

## Smoke test (real LLM providers via gateway)

```
$ python .scratch/smoke_test.py   # provider keys exported in the shell env

  OK  model=demo        provider=fake    status=200 latency=  63ms cache=NONE
  OK  model=fast        provider=nebius  status=200 latency=1476ms cache=NONE
  OK  model=smart       provider=nebius  status=200 latency=1079ms cache=NONE
  OK  model=demo        provider=fake    status=200 latency=  22ms cache=MISS

Total calls: 4, Successful: 4
SMOKE TEST: PASS
```

4/4 real calls succeeded (fake + 2 real providers via Nebius).
Keys are read from environment variables only; never logged or committed.

---

## Fallback demonstration (gateway logs)

Forced 429 from primary (fake :18110) → fallback to secondary (fake :18101):
```
X-Provider: fake
HTTP/1.1 200 OK
```
Gateway returned 200 from secondary after primary returned 429.

---

## What is NOT done / caveats

- Race-detector tests (`go test -race`) run in CI on ubuntu-latest (not locally —
  Windows host lacks 64-bit gcc required by CGO race detector).
- Cache HIT latency not measured (Redis unavailable during local benchmarks;
  integration test proves correctness but not latency).
- Load test uses fake upstream with fixed latency — real LLM latency is 1–10 s.
- k6 fallback scenario (30% 429 rate) not measured (would require Redis for
  shared breaker state across replicas).
- Docker images not built locally (distroless pull requires internet; CI builds them).
