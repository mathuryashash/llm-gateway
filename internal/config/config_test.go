package config

import (
	"os"
	"strings"
	"testing"
)

func TestDefaults(t *testing.T) {
	cfg := Defaults()
	if cfg.Server.Addr != ":18100" {
		t.Fatalf("expected :18100 got %s", cfg.Server.Addr)
	}
	if cfg.Breaker.FailureThreshold != 3 {
		t.Fatal("expected failure threshold 3")
	}
	if !cfg.RateLimit.FailOpen {
		t.Fatal("expected fail_open default true")
	}
}

func TestLoadEmpty(t *testing.T) {
	cfg, err := Load("")
	if err != nil {
		t.Fatalf("Load empty: %v", err)
	}
	if cfg.Server.Addr != ":18100" {
		t.Fatalf("expected :18100 got %s", cfg.Server.Addr)
	}
}

func TestLoadYAML(t *testing.T) {
	content := `
server:
  addr: ":18150"
providers:
  - name: groq
    base_url: https://api.groq.com/openai/v1
    key_env: GROQ_API_KEY
    timeout_sec: 30
routes:
  - alias: fast
    providers: [groq]
    models: [llama-3.1-8b-instant]
`
	f := t.TempDir() + "/config.yaml"
	if err := os.WriteFile(f, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(f)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Server.Addr != ":18150" {
		t.Fatalf("expected :18150 got %s", cfg.Server.Addr)
	}
	if len(cfg.Providers) != 1 || cfg.Providers[0].Name != "groq" {
		t.Fatal("provider not loaded")
	}
	if cfg.Providers[0].Timeout.Seconds() != 30 {
		t.Fatal("timeout not derived")
	}
}

func TestValidateMissingModel(t *testing.T) {
	content := `
providers:
  - name: groq
    base_url: https://api.groq.com/openai/v1
    key_env: GROQ_API_KEY
routes:
  - alias: fast
    providers: [groq]
    models: []
`
	f := t.TempDir() + "/bad.yaml"
	if err := os.WriteFile(f, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := Load(f)
	if err == nil {
		t.Fatal("expected error for mismatched models/providers")
	}
}

func TestEnvOverride(t *testing.T) {
	t.Setenv("GATEWAY_ADDR", ":19999")
	t.Setenv("GATEWAY_LOG_LEVEL", "debug")
	cfg, err := Load("")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Server.Addr != ":19999" {
		t.Fatalf("env override for addr expected :19999 got %s", cfg.Server.Addr)
	}
	if cfg.LogLevel != "debug" {
		t.Fatalf("env override for log_level expected debug got %s", cfg.LogLevel)
	}
}

func TestValidateUnknownProviderInRoute(t *testing.T) {
	content := `
providers:
  - name: groq
    base_url: https://api.groq.com/openai/v1
    key_env: GROQ_API_KEY
routes:
  - alias: fast
    providers: [groq, gorq]
    models: [llama-3.1-8b-instant, llama-3.1-8b-instant]
`
	f := t.TempDir() + "/typo.yaml"
	if err := os.WriteFile(f, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := Load(f)
	if err == nil || !strings.Contains(err.Error(), "gorq") {
		t.Fatalf("expected unknown-provider error naming gorq, got %v", err)
	}
}
