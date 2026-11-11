// Package breaker implements a per-route circuit breaker.
//
// States:
//   - Closed  : normal operation; failures counted.
//   - Open    : all requests rejected immediately; entered after
//               FailureThreshold consecutive failures.
//   - HalfOpen: one probe request allowed; success → Closed, failure → Open.
//
// The breaker is goroutine-safe.
package breaker

import (
	"sync"
	"time"
)

// State is the circuit breaker state.
type State int

const (
	StateClosed   State = iota // normal
	StateOpen                  // tripped; requests fail fast
	StateHalfOpen              // one test probe allowed
)

func (s State) String() string {
	switch s {
	case StateClosed:
		return "closed"
	case StateOpen:
		return "open"
	case StateHalfOpen:
		return "half_open"
	default:
		return "unknown"
	}
}

// Breaker is one circuit breaker for one upstream route.
type Breaker struct {
	mu               sync.Mutex
	state            State
	failures         int
	failureThreshold int
	recovery         time.Duration
	openedAt         time.Time
	// halfOpenInFlight tracks whether a probe is already in-flight.
	halfOpenInFlight bool
}

// New creates a Breaker with the given thresholds.
func New(failureThreshold int, recovery time.Duration) *Breaker {
	return &Breaker{
		failureThreshold: failureThreshold,
		recovery:         recovery,
	}
}

// Allow returns true when the caller may attempt a request.
// It transitions Open→HalfOpen when the recovery window has elapsed.
func (b *Breaker) Allow() bool {
	b.mu.Lock()
	defer b.mu.Unlock()

	switch b.state {
	case StateClosed:
		return true
	case StateOpen:
		if time.Since(b.openedAt) >= b.recovery {
			b.state = StateHalfOpen
			b.halfOpenInFlight = true
			return true
		}
		return false
	case StateHalfOpen:
		// Only one probe at a time.
		if b.halfOpenInFlight {
			return false
		}
		b.halfOpenInFlight = true
		return true
	}
	return false
}

// RecordSuccess records a successful call.
// HalfOpen → Closed; Closed resets failure count.
func (b *Breaker) RecordSuccess() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.failures = 0
	b.halfOpenInFlight = false
	b.state = StateClosed
}

// RecordFailure records a failed call.
// Closed: increment counter, trip when threshold reached.
// HalfOpen: immediately reopen.
func (b *Breaker) RecordFailure() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.halfOpenInFlight = false
	switch b.state {
	case StateClosed:
		b.failures++
		if b.failures >= b.failureThreshold {
			b.trip()
		}
	case StateHalfOpen:
		b.trip()
	}
}

// trip moves the breaker to Open and records the time.
// Caller must hold b.mu.
func (b *Breaker) trip() {
	b.state = StateOpen
	b.openedAt = time.Now()
}

// State returns the current state (for metrics / logging).
func (b *Breaker) State() State {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.state
}

// Registry keeps one Breaker per named route.
type Registry struct {
	mu       sync.Mutex
	breakers map[string]*Breaker
	cfg      Config
}

// Config parameterises all breakers in a registry.
type Config struct {
	FailureThreshold int
	Recovery         time.Duration
}

// NewRegistry creates an empty registry.
func NewRegistry(cfg Config) *Registry {
	return &Registry{
		breakers: make(map[string]*Breaker),
		cfg:      cfg,
	}
}

// Get returns (or lazily creates) the Breaker for a named route.
func (r *Registry) Get(name string) *Breaker {
	r.mu.Lock()
	defer r.mu.Unlock()
	if b, ok := r.breakers[name]; ok {
		return b
	}
	b := New(r.cfg.FailureThreshold, r.cfg.Recovery)
	r.breakers[name] = b
	return b
}
