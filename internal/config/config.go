// Package config loads, validates and hot-watches the gateway configuration.
// Config is a plain YAML file; every key can be overridden with an env var
// using the naming convention GATEWAY_<SECTION>_<KEY> (upper-snake).
package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// ProviderConfig is one upstream endpoint + its auth key env var.
type ProviderConfig struct {
	Name       string        `yaml:"name"`
	BaseURL    string        `yaml:"base_url"`
	KeyEnv     string        `yaml:"key_env"` // env var that holds the API key
	TimeoutSec int           `yaml:"timeout_sec"`
	Timeout    time.Duration `yaml:"-"` // derived
}

// RouteConfig maps a model alias to an ordered list of providers.
// The gateway tries them left to right, skipping circuit-open ones.
type RouteConfig struct {
	Alias     string   `yaml:"alias"`     // what clients send as "model"
	Providers []string `yaml:"providers"` // ordered provider names
	Models    []string `yaml:"models"`    // parallel list: provider[i] uses models[i]
}

// ClientKey is a hashed (SHA-256 hex) client credential.
type ClientKey struct {
	Name         string `yaml:"name"`
	HashedKey    string `yaml:"hashed_key"` // SHA-256 of the raw key, lowercase hex
	RequestsMin  int    `yaml:"requests_min"`
	DailyTokens  int    `yaml:"daily_tokens"`
}

// RateLimitConfig controls per-client throttling behaviour.
type RateLimitConfig struct {
	// FailOpen: when Redis is unavailable, allow traffic (true) or block (false).
	// Default true: gateway prefers availability over strict enforcement.
	FailOpen bool `yaml:"fail_open"`
}

// CacheConfig controls the response cache.
type CacheConfig struct {
	Enabled bool          `yaml:"enabled"`
	TTLSec  int           `yaml:"ttl_sec"`
	TTL     time.Duration `yaml:"-"`
}

// RedisConfig is the connection config for Redis.
type RedisConfig struct {
	Addr     string `yaml:"addr"`
	Password string `yaml:"password"`
	DB       int    `yaml:"db"`
}

// BreakerConfig controls per-route circuit breaker thresholds.
type BreakerConfig struct {
	// FailureThreshold: consecutive failures before opening.
	FailureThreshold int           `yaml:"failure_threshold"`
	// RecoverySec: how long to stay open before moving to half-open.
	RecoverySec      int           `yaml:"recovery_sec"`
	Recovery         time.Duration `yaml:"-"`
}

// ServerConfig holds HTTP server tuning knobs.
type ServerConfig struct {
	Addr            string `yaml:"addr"`
	MaxBodyBytes    int64  `yaml:"max_body_bytes"`
	ReadTimeoutSec  int    `yaml:"read_timeout_sec"`
	WriteTimeoutSec int    `yaml:"write_timeout_sec"`
	IdleTimeoutSec  int    `yaml:"idle_timeout_sec"`
}

// OTELConfig holds OpenTelemetry exporter settings (disabled by default).
type OTELConfig struct {
	Enabled     bool   `yaml:"enabled"`
	Endpoint    string `yaml:"endpoint"` // OTLP HTTP endpoint
	ServiceName string `yaml:"service_name"`
}

// Config is the top-level configuration object.
type Config struct {
	Server     ServerConfig     `yaml:"server"`
	Redis      RedisConfig      `yaml:"redis"`
	Providers  []ProviderConfig `yaml:"providers"`
	Routes     []RouteConfig    `yaml:"routes"`
	ClientKeys []ClientKey      `yaml:"client_keys"`
	RateLimit  RateLimitConfig  `yaml:"rate_limit"`
	Cache      CacheConfig      `yaml:"cache"`
	Breaker    BreakerConfig    `yaml:"breaker"`
	OTEL       OTELConfig       `yaml:"otel"`
	LogLevel   string           `yaml:"log_level"`
}

// Defaults returns a Config with safe defaults.
func Defaults() Config {
	return Config{
		Server: ServerConfig{
			Addr:            ":18100",
			MaxBodyBytes:    1 << 20, // 1 MB
			ReadTimeoutSec:  30,
			WriteTimeoutSec: 120,
			IdleTimeoutSec:  60,
		},
		Redis: RedisConfig{
			Addr: "localhost:18102",
			DB:   0,
		},
		RateLimit: RateLimitConfig{
			FailOpen: true, // prefer availability
		},
		Cache: CacheConfig{
			Enabled: true,
			TTLSec:  300,
		},
		Breaker: BreakerConfig{
			FailureThreshold: 3,
			RecoverySec:      30,
		},
		OTEL: OTELConfig{
			ServiceName: "llm-gateway",
		},
		LogLevel: "info",
	}
}

// Load reads a YAML file and merges env overrides.
// If path is empty, only defaults + env are used.
func Load(path string) (*Config, error) {
	cfg := Defaults()

	if path != "" {
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("config: read %s: %w", path, err)
		}
		if err := yaml.Unmarshal(data, &cfg); err != nil {
			return nil, fmt.Errorf("config: parse %s: %w", path, err)
		}
	}

	applyEnvOverrides(&cfg)
	derive(&cfg)

	if err := validate(&cfg); err != nil {
		return nil, err
	}
	return &cfg, nil
}

// applyEnvOverrides applies simple env-variable overrides for key knobs.
func applyEnvOverrides(cfg *Config) {
	if v := os.Getenv("GATEWAY_ADDR"); v != "" {
		cfg.Server.Addr = v
	}
	if v := os.Getenv("GATEWAY_REDIS_ADDR"); v != "" {
		cfg.Redis.Addr = v
	}
	if v := os.Getenv("GATEWAY_REDIS_PASSWORD"); v != "" {
		cfg.Redis.Password = v
	}
	if v := os.Getenv("GATEWAY_LOG_LEVEL"); v != "" {
		cfg.LogLevel = v
	}
	if v := os.Getenv("GATEWAY_CACHE_ENABLED"); v != "" {
		cfg.Cache.Enabled = strings.ToLower(v) == "true"
	}
	if v := os.Getenv("GATEWAY_RATELIMIT_FAILOPEN"); v != "" {
		cfg.RateLimit.FailOpen = strings.ToLower(v) == "true"
	}
	if v := os.Getenv("GATEWAY_MAX_BODY_BYTES"); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil {
			cfg.Server.MaxBodyBytes = n
		}
	}
}

// derive populates computed Duration fields from integer seconds.
func derive(cfg *Config) {
	cfg.Cache.TTL = time.Duration(cfg.Cache.TTLSec) * time.Second
	cfg.Breaker.Recovery = time.Duration(cfg.Breaker.RecoverySec) * time.Second
	for i := range cfg.Providers {
		if cfg.Providers[i].TimeoutSec == 0 {
			cfg.Providers[i].TimeoutSec = 60
		}
		cfg.Providers[i].Timeout = time.Duration(cfg.Providers[i].TimeoutSec) * time.Second
	}
}

// validate checks required fields and consistency.
func validate(cfg *Config) error {
	for i, p := range cfg.Providers {
		if p.Name == "" {
			return fmt.Errorf("config: provider[%d] missing name", i)
		}
		if p.BaseURL == "" {
			return fmt.Errorf("config: provider %q missing base_url", p.Name)
		}
	}
	for i, r := range cfg.Routes {
		if r.Alias == "" {
			return fmt.Errorf("config: route[%d] missing alias", i)
		}
		if len(r.Providers) == 0 {
			return fmt.Errorf("config: route %q has no providers", r.Alias)
		}
		if len(r.Models) != len(r.Providers) {
			return fmt.Errorf("config: route %q: providers and models list length mismatch", r.Alias)
		}
	}
	return nil
}
