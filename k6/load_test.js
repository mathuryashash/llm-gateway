// k6 load test for llm-gateway
// Run: k6 run k6/load_test.js
// Gateway must be running on :18100 with fakeupstream on :18101

import http from "k6/http";
import { check, sleep } from "k6";
import { Trend, Counter, Rate } from "k6/metrics";

const gatewayLatency = new Trend("gateway_latency_ms");
const directLatency = new Trend("direct_latency_ms");
const errorRate = new Rate("error_rate");
const fallbackRate = new Rate("fallback_rate");
const cacheHitRate = new Rate("cache_hit_rate");

const GATEWAY = "http://localhost:18100";
const UPSTREAM = "http://localhost:18101";

const CHAT_BODY = JSON.stringify({
  model: "demo",
  messages: [{ role: "user", content: "hello" }],
  temperature: 1.0,
});

const CACHE_BODY = JSON.stringify({
  model: "demo",
  messages: [{ role: "user", content: "cache benchmark fixed" }],
  temperature: 0.0,
});

export const options = {
  scenarios: {
    // Scenario 1: measure gateway overhead at low concurrency
    overhead_low: {
      executor: "constant-vus",
      vus: 5,
      duration: "10s",
      exec: "measureOverhead",
      startTime: "0s",
    },
    // Scenario 2: measure at higher concurrency
    overhead_high: {
      executor: "constant-vus",
      vus: 20,
      duration: "10s",
      exec: "measureOverhead",
      startTime: "12s",
    },
    // Scenario 3: cache HIT latency
    cache_hit: {
      executor: "constant-vus",
      vus: 5,
      duration: "10s",
      exec: "measureCacheHit",
      startTime: "24s",
    },
  },
};

export function measureOverhead() {
  // Gateway request
  const gwStart = Date.now();
  const gwResp = http.post(
    GATEWAY + "/v1/chat/completions",
    CHAT_BODY,
    { headers: { "Content-Type": "application/json" } }
  );
  const gwLatency = Date.now() - gwStart;

  check(gwResp, { "gateway 200": (r) => r.status === 200 });
  errorRate.add(gwResp.status !== 200);
  gatewayLatency.add(gwLatency);

  // Direct upstream request (baseline)
  const directStart = Date.now();
  const directResp = http.post(
    UPSTREAM + "/chat/completions",
    CHAT_BODY,
    { headers: { "Content-Type": "application/json", "Authorization": "Bearer test" } }
  );
  const dLatency = Date.now() - directStart;
  directLatency.add(dLatency);
}

export function measureCacheHit() {
  const resp = http.post(
    GATEWAY + "/v1/chat/completions",
    CACHE_BODY,
    { headers: { "Content-Type": "application/json" } }
  );
  check(resp, { "cache 200": (r) => r.status === 200 });
  errorRate.add(resp.status !== 200);
  cacheHitRate.add(resp.headers["X-Cache"] === "HIT");
  gatewayLatency.add(resp.timings.duration);
}
