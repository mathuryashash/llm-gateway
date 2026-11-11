package cache

import (
	"context"
	"testing"
)

func TestKeyStability(t *testing.T) {
	rk := RequestKey{
		Model:       "gpt-4",
		Messages:    []msgKey{{Role: "user", Content: "hello"}},
		Temperature: 0,
		MaxTokens:   100,
	}
	k1 := Key(rk)
	k2 := Key(rk)
	if k1 != k2 {
		t.Fatal("key should be stable across calls")
	}
	if len(k1) == 0 {
		t.Fatal("key should not be empty")
	}
}

func TestKeyDifferentForDifferentInputs(t *testing.T) {
	rk1 := RequestKey{Model: "a", Messages: []msgKey{{Role: "user", Content: "hi"}}}
	rk2 := RequestKey{Model: "b", Messages: []msgKey{{Role: "user", Content: "hi"}}}
	if Key(rk1) == Key(rk2) {
		t.Fatal("different models should produce different keys")
	}
}

func TestIsDeterministic(t *testing.T) {
	zero := 0.0
	one := 1.0

	if !IsDeterministic(&zero, false) {
		t.Fatal("temperature=0, stream=false should be deterministic")
	}
	if IsDeterministic(&one, false) {
		t.Fatal("temperature=1 should not be deterministic")
	}
	if IsDeterministic(&zero, true) {
		t.Fatal("stream=true should not be deterministic")
	}
	if IsDeterministic(nil, false) {
		t.Fatal("nil temperature should not be deterministic")
	}
}

// TestGetMissNilRedis verifies that a nil-Redis cache returns a miss without error.
func TestGetMissNilRedis(t *testing.T) {
	c := New(nil, 0)
	resp, err := c.Get(context.TODO(), "somekey")
	if err != nil {
		t.Fatal(err)
	}
	if resp != nil {
		t.Fatal("expected nil on miss")
	}
}

// TestSetNilRedis verifies that Set with nil Redis is a no-op.
func TestSetNilRedis(t *testing.T) {
	c := New(nil, 0)
	err := c.Set(context.TODO(), "k", &CachedResponse{StatusCode: 200})
	if err != nil {
		t.Fatal(err)
	}
}
