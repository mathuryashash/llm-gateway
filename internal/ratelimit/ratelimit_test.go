package ratelimit

import (
	"context"
	"testing"
)

func TestHashedKey(t *testing.T) {
	k1 := HashedKey("test-key")
	k2 := HashedKey("test-key")
	k3 := HashedKey("other-key")
	if k1 != k2 {
		t.Fatal("same input should produce same hash")
	}
	if k1 == k3 {
		t.Fatal("different input should produce different hash")
	}
	if len(k1) != 64 {
		t.Fatalf("expected 64 hex chars, got %d", len(k1))
	}
}

// TestInMemoryRPM exercises the in-memory fallback token bucket.
func TestInMemoryRPM(t *testing.T) {
	// Nil Redis → pure in-memory path.
	l := New(nil, true)
	ctx := context.Background()

	hash := HashedKey("user1")

	// 3 requests/min capacity.
	for i := 0; i < 3; i++ {
		d, err := l.Allow(ctx, hash, 3, 0, 0)
		if err != nil {
			t.Fatal(err)
		}
		if d != Allowed {
			t.Fatalf("request %d should be allowed, got %s", i, d)
		}
	}

	// 4th should be denied.
	d, err := l.Allow(ctx, hash, 3, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if d != DeniedRPM {
		t.Fatalf("4th request should be denied, got %s", d)
	}
}

func TestDecisionString(t *testing.T) {
	cases := []struct {
		d Decision
		s string
	}{
		{Allowed, "allowed"},
		{DeniedRPM, "denied_rpm"},
		{DeniedDaily, "denied_daily"},
	}
	for _, tc := range cases {
		if tc.d.String() != tc.s {
			t.Fatalf("expected %s got %s", tc.s, tc.d.String())
		}
	}
}

func TestUtcDay(t *testing.T) {
	day := utcDay()
	if len(day) != 10 {
		t.Fatalf("expected YYYY-MM-DD (10 chars), got %q", day)
	}
}

func TestSecondsToMidnight(t *testing.T) {
	s := secondsToMidnight()
	if s <= 0 || s > 86400 {
		t.Fatalf("unexpected value %d", s)
	}
}
