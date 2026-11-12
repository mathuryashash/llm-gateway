package breaker

import (
	"testing"
	"time"
)

func TestClosedAllowsAll(t *testing.T) {
	b := New(3, time.Second)
	for i := 0; i < 10; i++ {
		if !b.Allow() {
			t.Fatal("closed breaker should allow")
		}
	}
}

func TestTripsAfterThreshold(t *testing.T) {
	b := New(3, time.Second)
	for i := 0; i < 2; i++ {
		b.Allow()
		b.RecordFailure()
		if b.State() != StateClosed {
			t.Fatalf("should still be closed after %d failures", i+1)
		}
	}
	b.Allow()
	b.RecordFailure() // 3rd failure
	if b.State() != StateOpen {
		t.Fatal("should be open after 3 failures")
	}
	if b.Allow() {
		t.Fatal("open breaker should not allow")
	}
}

func TestRecoveryTransition(t *testing.T) {
	b := New(1, 50*time.Millisecond)
	b.Allow()
	b.RecordFailure()
	if b.State() != StateOpen {
		t.Fatal("expected open")
	}
	// still open immediately
	if b.Allow() {
		t.Fatal("should be blocked while open")
	}
	time.Sleep(60 * time.Millisecond)
	// should transition to half-open
	if !b.Allow() {
		t.Fatal("should allow probe after recovery window")
	}
	if b.State() != StateHalfOpen {
		t.Fatal("expected half-open")
	}
	// success → closed
	b.RecordSuccess()
	if b.State() != StateClosed {
		t.Fatal("expected closed after success")
	}
}

func TestHalfOpenOnlyOneProbe(t *testing.T) {
	b := New(1, 50*time.Millisecond)
	b.Allow()
	b.RecordFailure()
	time.Sleep(60 * time.Millisecond)
	// First Allow moves to half-open with probe in-flight
	if !b.Allow() {
		t.Fatal("should allow first probe")
	}
	// Second Allow should be blocked (probe in-flight)
	if b.Allow() {
		t.Fatal("should block second probe while one is in-flight")
	}
}

func TestHalfOpenFailureReopens(t *testing.T) {
	b := New(1, 50*time.Millisecond)
	b.Allow()
	b.RecordFailure()
	time.Sleep(60 * time.Millisecond)
	b.Allow()         // enter half-open
	b.RecordFailure() // fail the probe → back to open
	if b.State() != StateOpen {
		t.Fatal("expected open after half-open failure")
	}
}

func TestSuccessResetsFailureCount(t *testing.T) {
	b := New(3, time.Second)
	for i := 0; i < 2; i++ {
		b.Allow()
		b.RecordFailure()
	}
	b.Allow()
	b.RecordSuccess() // reset
	// 3 more failures should be needed to trip
	for i := 0; i < 2; i++ {
		b.Allow()
		b.RecordFailure()
	}
	if b.State() != StateClosed {
		t.Fatal("should still be closed; counter was reset")
	}
}

func TestRegistry(t *testing.T) {
	reg := NewRegistry(Config{FailureThreshold: 2, Recovery: time.Second})
	b1 := reg.Get("route-a")
	b2 := reg.Get("route-a")
	if b1 != b2 {
		t.Fatal("same key should return same breaker")
	}
	b3 := reg.Get("route-b")
	if b1 == b3 {
		t.Fatal("different keys should return different breakers")
	}
}
