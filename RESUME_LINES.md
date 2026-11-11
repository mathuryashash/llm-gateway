# Resume Lines

## Bullet points

- Built Go LLM gateway with OpenAI-compatible API, circuit breaking, Redis-backed
  token-bucket rate limiting, and response caching; adds ~1 ms p50 / ~3 ms p95 over a
  direct call (k6, 20 VUs, fake upstream, laptop).

- Implemented per-route circuit breaker (closed/open/half-open) and an ordered fallback
  chain; integration tests cover 429 -> fallback and Retry-After.

- Used atomic Redis Lua scripts for shared rate-limit state across replicas;
  integration-tested with testcontainers-go Redis (2 gateway instances, shared limit).

## Skills this repo proves (only what code actually uses and tests)

**Go**: net/http, log/slog, context propagation, goroutines, mutexes, table-driven
tests, fuzz testing (go test -fuzz), benchmarks (-bench/-benchmem), race detector
(in CI).

**Distributed systems patterns**: circuit breaker (closed/open/half-open), token
bucket rate limiting, response caching with determinism check, graceful degradation
(fail-open), Retry-After header handling.

**Redis**: Lua scripts for atomic operations, testcontainers-go integration tests,
in-memory fallback design.

**Observability**: Prometheus metrics (counters, histograms, gauges), structured JSON
logging (slog), OpenTelemetry OTLP trace exporter (off by default).

**Testing**: unit tests for every package, integration tests with Docker Redis,
fuzz test for cache key normalisation, k6 load tests with custom metrics.

**CI/CD**: GitHub Actions workflow (go build, vet, staticcheck, race tests,
integration, fuzz, docker build), actionlint-validated.

**Docker**: multi-stage build, distroless non-root final image.
