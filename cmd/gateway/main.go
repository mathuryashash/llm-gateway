// Command gateway is the LLM gateway entry point.
// It loads config, wires dependencies, starts the HTTP server,
// and handles graceful shutdown on SIGINT/SIGTERM.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/redis/go-redis/v9"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/sdk/trace"

	"github.com/yashash-pushkar/llm-gateway/internal/breaker"
	"github.com/yashash-pushkar/llm-gateway/internal/cache"
	"github.com/yashash-pushkar/llm-gateway/internal/config"
	"github.com/yashash-pushkar/llm-gateway/internal/middleware"
	"github.com/yashash-pushkar/llm-gateway/internal/provider"
	"github.com/yashash-pushkar/llm-gateway/internal/proxy"
	"github.com/yashash-pushkar/llm-gateway/internal/ratelimit"
	"github.com/yashash-pushkar/llm-gateway/internal/router"
)

func main() {
	cfgPath := flag.String("config", "", "path to config.yaml (optional; env vars override)")
	flag.Parse()

	// --- Logger ---
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))

	// --- Config ---
	cfg, err := config.Load(*cfgPath)
	if err != nil {
		logger.Error("config load failed", "err", err)
		os.Exit(1)
	}

	// Set log level from config.
	var level slog.Level
	switch cfg.LogLevel {
	case "debug":
		level = slog.LevelDebug
	case "warn":
		level = slog.LevelWarn
	case "error":
		level = slog.LevelError
	default:
		level = slog.LevelInfo
	}
	logger = slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: level}))
	slog.SetDefault(logger)

	// --- OTEL (optional) ---
	if cfg.OTEL.Enabled {
		tp, err := initTracer(cfg)
		if err != nil {
			logger.Warn("OTEL init failed, continuing without traces", "err", err)
		} else {
			otel.SetTracerProvider(tp)
			defer func() {
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				_ = tp.Shutdown(ctx)
			}()
		}
	}

	// --- Redis ---
	rdb := redis.NewClient(&redis.Options{
		Addr:     cfg.Redis.Addr,
		Password: cfg.Redis.Password,
		DB:       cfg.Redis.DB,
	})
	pingCtx, pingCancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer pingCancel()
	if err := rdb.Ping(pingCtx).Err(); err != nil {
		logger.Warn("Redis unavailable; rate limiting uses in-memory fallback, caching disabled", "err", err)
		rdb = nil
	}

	// --- Provider clients ---
	clients := make(map[string]*provider.Client)
	for _, pc := range cfg.Providers {
		key := os.Getenv(pc.KeyEnv)
		if key == "" {
			logger.Warn("provider key env var not set", "provider", pc.Name, "env", pc.KeyEnv)
		}
		clients[pc.Name] = provider.New(pc.Name, pc.BaseURL, key, pc.Timeout)
	}

	// --- Router ---
	r, err := router.New(cfg, clients)
	if err != nil {
		logger.Error("router init failed", "err", err)
		os.Exit(1)
	}

	// --- Breakers ---
	br := breaker.NewRegistry(breaker.Config{
		FailureThreshold: cfg.Breaker.FailureThreshold,
		Recovery:         cfg.Breaker.Recovery,
	})

	// --- Rate limiter ---
	var limRDB *redis.Client
	if rdb != nil {
		limRDB = rdb
	}
	lim := ratelimit.New(limRDB, cfg.RateLimit.FailOpen)

	// --- Cache ---
	var cacheRDB *redis.Client
	if rdb != nil && cfg.Cache.Enabled {
		cacheRDB = rdb
	}
	c := cache.New(cacheRDB, cfg.Cache.TTL)

	// --- Handler ---
	proxyHandler := proxy.New(r, br, lim, c, logger, cfg)

	// --- Mux ---
	mux := http.NewServeMux()
	mux.Handle("/metrics", promhttp.Handler())
	mux.Handle("/", applyMiddleware(proxyHandler, cfg, logger))

	// --- Server ---
	srv := &http.Server{
		Addr:         cfg.Server.Addr,
		Handler:      mux,
		ReadTimeout:  time.Duration(cfg.Server.ReadTimeoutSec) * time.Second,
		WriteTimeout: time.Duration(cfg.Server.WriteTimeoutSec) * time.Second,
		IdleTimeout:  time.Duration(cfg.Server.IdleTimeoutSec) * time.Second,
	}

	// --- Graceful shutdown ---
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)

	go func() {
		logger.Info("gateway starting", "addr", cfg.Server.Addr)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			logger.Error("server error", "err", err)
			os.Exit(1)
		}
	}()

	<-quit
	logger.Info("shutting down")
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := srv.Shutdown(ctx); err != nil {
		logger.Error("shutdown error", "err", err)
	}
	logger.Info("shutdown complete")
}

func applyMiddleware(h http.Handler, cfg *config.Config, logger *slog.Logger) http.Handler {
	h = middleware.Auth(cfg.ClientKeys)(h)
	h = middleware.MaxBodySize(cfg.Server.MaxBodyBytes)(h)
	h = middleware.Logger(logger)(h)
	h = middleware.RequestID(h)
	return h
}

func initTracer(cfg *config.Config) (*trace.TracerProvider, error) {
	ctx := context.Background()
	exp, err := otlptracehttp.New(ctx,
		otlptracehttp.WithEndpoint(cfg.OTEL.Endpoint),
		otlptracehttp.WithInsecure(),
	)
	if err != nil {
		return nil, fmt.Errorf("OTLP exporter: %w", err)
	}
	tp := trace.NewTracerProvider(trace.WithBatcher(exp))
	return tp, nil
}
