// Package proxy implements the core HTTP handler for the LLM gateway.
// It orchestrates routing, circuit breaking, rate limiting, caching,
// fallback, and observability for POST /v1/chat/completions.
package proxy

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/mathuryashash/llm-gateway/internal/breaker"
	"github.com/mathuryashash/llm-gateway/internal/cache"
	"github.com/mathuryashash/llm-gateway/internal/config"
	"github.com/mathuryashash/llm-gateway/internal/metrics"
	"github.com/mathuryashash/llm-gateway/internal/middleware"
	"github.com/mathuryashash/llm-gateway/internal/ratelimit"
	"github.com/mathuryashash/llm-gateway/internal/router"
	"github.com/mathuryashash/llm-gateway/internal/types"
)

// RouterIface is the interface the proxy uses so tests can inject mocks.
type RouterIface interface {
	Resolve(alias string) ([]router.Route, error)
	KnownAliases() []string
}

// Handler is the main gateway HTTP handler.
type Handler struct {
	router   RouterIface
	breakers *breaker.Registry
	limiter  *ratelimit.Limiter
	cache    *cache.Cache
	logger   *slog.Logger
	cfg      *config.Config
}

// New creates a Handler.
func New(
	r RouterIface,
	br *breaker.Registry,
	lim *ratelimit.Limiter,
	c *cache.Cache,
	logger *slog.Logger,
	cfg *config.Config,
) *Handler {
	return &Handler{
		router:   r,
		breakers: br,
		limiter:  lim,
		cache:    c,
		logger:   logger,
		cfg:      cfg,
	}
}

// ServeHTTP routes incoming requests to the appropriate handler.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/chat/completions", h.handleChat)
	mux.HandleFunc("GET /v1/models", h.handleModels)
	mux.HandleFunc("GET /healthz", h.handleHealthz)
	mux.HandleFunc("GET /readyz", h.handleReadyz)
	mux.ServeHTTP(w, r)
}

// handleChat handles POST /v1/chat/completions.
func (h *Handler) handleChat(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	ctx := r.Context()
	reqID, _ := ctx.Value(middleware.CtxRequestID).(string)

	// --- Parse request ---
	body, err := io.ReadAll(r.Body)
	if err != nil {
		h.writeError(w, http.StatusBadRequest, "failed to read body", "bad_request")
		return
	}
	var req types.ChatCompletionRequest
	if err := json.Unmarshal(body, &req); err != nil {
		h.writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error(), "bad_request")
		return
	}
	if req.Model == "" {
		h.writeError(w, http.StatusBadRequest, "model field is required", "bad_request")
		return
	}

	// --- Resolve routes ---
	routes, err := h.router.Resolve(req.Model)
	if err != nil {
		h.writeError(w, http.StatusNotFound, fmt.Sprintf("model %q not found", req.Model), "model_not_found")
		return
	}

	// --- Rate limiting ---
	clientHash, _ := ctx.Value(middleware.CtxClientHash).(string)
	if clientHash != "" {
		ck := h.findClientKey(clientHash)
		if ck != nil {
			d, err := h.limiter.Allow(ctx, clientHash, ck.RequestsMin, ck.DailyTokens, 0)
			if err != nil {
				h.logger.Error("rate limiter error", "err", err, "req_id", reqID)
			}
			if d != ratelimit.Allowed {
				metrics.RateLimitedTotal.WithLabelValues(d.String()).Inc()
				w.Header().Set("Retry-After", "60")
				h.writeError(w, http.StatusTooManyRequests, "rate limit exceeded", d.String())
				return
			}
		}
	}

	// --- Cache check (non-streaming only) ---
	var cacheKey string
	if !req.Stream && h.cfg.Cache.Enabled && cache.IsDeterministic(req.Temperature, req.Stream) {
		rk := buildCacheKey(&req)
		cacheKey = cache.Key(rk)
		if hit, err := h.cache.Get(ctx, cacheKey); err == nil && hit != nil {
			metrics.CacheHitsTotal.Inc()
			w.Header().Set("X-Cache", "HIT")
			w.Header().Set("X-Provider", hit.Provider)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(hit.StatusCode)
			_, _ = w.Write(hit.Body)
			elapsed := time.Since(start).Seconds()
			metrics.RequestDurationSecs.WithLabelValues(req.Model, hit.Provider).Observe(elapsed)
			return
		}
		metrics.CacheMissesTotal.Inc()
		w.Header().Set("X-Cache", "MISS")
	}

	// --- Fallback chain ---
	if req.Stream {
		h.handleStream(ctx, w, r, &req, routes, reqID, start)
		return
	}

	var lastErr error
	for i, route := range routes {
		b := h.breakers.Get(route.RouteKey)
		// Update breaker metric.
		metrics.BreakerState.WithLabelValues(route.RouteKey).Set(float64(b.State()))

		if !b.Allow() {
			h.logger.Debug("breaker open, skipping", "route", route.RouteKey, "req_id", reqID)
			if i < len(routes)-1 {
				metrics.FallbacksTotal.WithLabelValues(route.RouteKey, "breaker_open").Inc()
			}
			continue
		}

		upstreamStart := time.Now()
		result, err := route.Client.Chat(ctx, route.Model, &req)
		upstreamLatency := time.Since(upstreamStart).Seconds()

		if err != nil {
			b.RecordFailure()
			metrics.FallbacksTotal.WithLabelValues(route.RouteKey, "error").Inc()
			metrics.UpstreamDurationSecs.WithLabelValues(route.RouteKey, "error").Observe(upstreamLatency)
			lastErr = err
			h.logger.Warn("upstream error", "route", route.RouteKey, "err", err, "req_id", reqID)
			continue
		}

		statusStr := strconv.Itoa(result.StatusCode)
		metrics.UpstreamDurationSecs.WithLabelValues(route.RouteKey, statusStr).Observe(upstreamLatency)

		if result.StatusCode == http.StatusTooManyRequests || result.StatusCode >= 500 {
			b.RecordFailure()
			if i < len(routes)-1 {
				reason := "upstream_429"
				if result.StatusCode >= 500 {
					reason = "upstream_5xx"
				}
				metrics.FallbacksTotal.WithLabelValues(route.RouteKey, reason).Inc()
			}
			if result.RetryAfter > 0 {
				h.logger.Info("upstream rate limited, respecting Retry-After",
					"route", route.RouteKey, "retry_after", result.RetryAfter, "req_id", reqID)
			}
			lastErr = fmt.Errorf("upstream %s: status %d", route.RouteKey, result.StatusCode)
			h.logger.Warn("upstream non-2xx", "route", route.RouteKey, "status", result.StatusCode, "req_id", reqID)
			continue
		}

		// Success.
		b.RecordSuccess()
		metrics.RequestsTotal.WithLabelValues(req.Model, route.Client.Name(), statusStr).Inc()
		elapsed := time.Since(start).Seconds()
		metrics.RequestDurationSecs.WithLabelValues(req.Model, route.Client.Name()).Observe(elapsed)

		if result.Response != nil {
			metrics.TokensTotal.WithLabelValues(route.Client.Name(), "prompt").Add(float64(result.Response.Usage.PromptTokens))
			metrics.TokensTotal.WithLabelValues(route.Client.Name(), "completion").Add(float64(result.Response.Usage.CompletionTokens))
		}

		// Cache the response.
		if cacheKey != "" && result.Body != nil {
			_ = h.cache.Set(ctx, cacheKey, &cache.CachedResponse{
				Body:       result.Body,
				StatusCode: result.StatusCode,
				Provider:   route.Client.Name(),
			})
		}

		w.Header().Set("X-Provider", route.Client.Name())
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(result.StatusCode)
		_, _ = w.Write(result.Body)
		return
	}

	// All routes failed.
	h.logger.Error("all routes failed", "model", req.Model, "last_err", lastErr, "req_id", reqID)
	metrics.RequestsTotal.WithLabelValues(req.Model, "none", "502").Inc()
	h.writeError(w, http.StatusBadGateway, "all upstream providers failed", "upstream_error")
}

// handleStream handles streaming requests.
// SAFETY RULE: once SSE bytes are written to the client we cannot retry on a
// different provider — partial data has already been delivered. We try the
// next provider only if the upstream returned a non-2xx status BEFORE sending
// any SSE bytes.
func (h *Handler) handleStream(
	ctx context.Context,
	w http.ResponseWriter,
	r *http.Request,
	req *types.ChatCompletionRequest,
	routes []router.Route,
	reqID string,
	start time.Time,
) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		h.writeError(w, http.StatusInternalServerError, "streaming not supported", "internal")
		return
	}

	for i, route := range routes {
		b := h.breakers.Get(route.RouteKey)
		if !b.Allow() {
			if i < len(routes)-1 {
				metrics.FallbacksTotal.WithLabelValues(route.RouteKey, "breaker_open").Inc()
			}
			continue
		}

		bytesSent := 0
		upstreamStart := time.Now()
		statusCode, err := route.Client.ChatStream(ctx, route.Model, req, func(line string) error {
			if bytesSent == 0 {
				// First byte — commit to this provider.
				w.Header().Set("Content-Type", "text/event-stream")
				w.Header().Set("Cache-Control", "no-cache")
				w.Header().Set("X-Provider", route.Client.Name())
				w.WriteHeader(http.StatusOK)
			}
			bytesSent += len(line) + 1
			_, writeErr := fmt.Fprintln(w, line)
			if writeErr == nil {
				flusher.Flush()
			}
			return writeErr
		})
		upstreamLatency := time.Since(upstreamStart).Seconds()
		metrics.UpstreamDurationSecs.WithLabelValues(route.RouteKey, strconv.Itoa(statusCode)).Observe(upstreamLatency)

		if bytesSent > 0 {
			// Bytes already sent — cannot retry.
			if err != nil {
				h.logger.Error("stream error after bytes sent", "err", err, "req_id", reqID)
			}
			b.RecordSuccess()
			metrics.RequestDurationSecs.WithLabelValues(req.Model, route.Client.Name()).Observe(time.Since(start).Seconds())
			return
		}

		if err != nil || statusCode >= 400 {
			b.RecordFailure()
			if i < len(routes)-1 {
				metrics.FallbacksTotal.WithLabelValues(route.RouteKey, "stream_error").Inc()
			}
			continue
		}
	}
}

func (h *Handler) handleModels(w http.ResponseWriter, r *http.Request) {
	type modelObj struct {
		ID     string `json:"id"`
		Object string `json:"object"`
	}
	type modelsResp struct {
		Object string      `json:"object"`
		Data   []modelObj  `json:"data"`
	}
	aliases := h.router.KnownAliases()
	data := make([]modelObj, len(aliases))
	for i, a := range aliases {
		data[i] = modelObj{ID: a, Object: "model"}
	}
	resp := modelsResp{Object: "list", Data: data}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}

func (h *Handler) handleHealthz(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok"))
}

func (h *Handler) handleReadyz(w http.ResponseWriter, r *http.Request) {
	// Could check Redis ping etc.; for now mirror healthz.
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ready"))
}

func (h *Handler) writeError(w http.ResponseWriter, code int, msg, errType string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	resp := types.ErrorResponse{
		Error: types.ErrorDetail{Message: msg, Type: errType},
	}
	_ = json.NewEncoder(w).Encode(resp)
}

func (h *Handler) findClientKey(hash string) *config.ClientKey {
	for i := range h.cfg.ClientKeys {
		if h.cfg.ClientKeys[i].HashedKey == hash {
			return &h.cfg.ClientKeys[i]
		}
	}
	return nil
}

func buildCacheKey(req *types.ChatCompletionRequest) cache.RequestKey {
	msgs := make([]cache.MsgKey, len(req.Messages))
	for i, m := range req.Messages {
		content := ""
		switch v := m.Content.(type) {
		case string:
			content = v
		}
		msgs[i] = cache.MsgKey{Role: m.Role, Content: content}
	}
	temp := 0.0
	if req.Temperature != nil {
		temp = *req.Temperature
	}
	topP := 0.0
	if req.TopP != nil {
		topP = *req.TopP
	}
	return cache.RequestKey{
		Model:       req.Model,
		Messages:    msgs,
		Temperature: temp,
		TopP:        topP,
		MaxTokens:   req.MaxTokens,
	}
}
