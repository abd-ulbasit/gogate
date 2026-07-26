package circuitbreaker

import (
	"errors"
	"sync"
	"sync/atomic"
	"time"
)

// Circuit Breaker protects backends from cascading failures.
//
// States:
// - CLOSED: Normal operation, requests pass through
// - OPEN: Failures exceeded threshold, requests fail immediately
// - HALF-OPEN: Timeout elapsed, a bounded number of probe requests are admitted
//
// State transitions:
//   CLOSED --[failures >= threshold in window]--> OPEN
//   OPEN   --[timeout expired]-----------------> HALF-OPEN
//   HALF-OPEN --[SuccessThreshold successes]---> CLOSED
//   HALF-OPEN --[any failure]------------------> OPEN
//
// Why circuit breaker?
// 1. Fail fast - don't wait for timeout on known-bad backend
// 2. Give backend time to recover
// 3. Prevent cascading failures across services
//
// Concurrency note: the half-open state is admission-controlled, not just a
// state label. Under real load, every caller blocked behind an open circuit
// arrives at Allow() within microseconds of the timeout expiring. If half-open
// simply returned nil, the first thing a backend saw after being declared dead
// would be the entire backlog at once - the stampede the breaker exists to
// prevent. MaxHalfOpenRequests caps the number of probes in flight; everyone
// else keeps getting ErrCircuitOpen until a probe reports back.

// State represents the circuit breaker state.
type State int

const (
	StateClosed   State = iota // Normal operation
	StateOpen                  // Blocking all requests
	StateHalfOpen              // Testing if backend recovered
)

func (s State) String() string {
	switch s {
	case StateClosed:
		return "closed"
	case StateOpen:
		return "open"
	case StateHalfOpen:
		return "half-open"
	default:
		return "unknown"
	}
}

// Common errors
var (
	ErrCircuitOpen = errors.New("circuit breaker is open")
)

// Config holds circuit breaker configuration.
type Config struct {
	// FailureThreshold: failures before opening circuit (default: 5)
	FailureThreshold int

	// SuccessThreshold: successes in half-open before closing (default: 1)
	SuccessThreshold int

	// MaxHalfOpenRequests: probes admitted concurrently in half-open
	// (default: SuccessThreshold). Admitting exactly SuccessThreshold probes
	// lets a healthy backend close the circuit in one round trip instead of
	// SuccessThreshold sequential ones, while still bounding the load a
	// recovering backend receives. Set to 1 for the most cautious probe.
	MaxHalfOpenRequests int

	// Timeout: how long to stay open before half-open (default: 30s)
	Timeout time.Duration

	// Window: time window for counting failures (default: 60s)
	Window time.Duration
}

// DefaultConfig returns sensible defaults.
func DefaultConfig() Config {
	return Config{
		FailureThreshold:    5,
		SuccessThreshold:    1,
		MaxHalfOpenRequests: 1,
		Timeout:             30 * time.Second,
		Window:              60 * time.Second,
	}
}

// CircuitBreaker implements the circuit breaker pattern.
//
// Thread-safe: All methods are safe for concurrent use.
type CircuitBreaker struct {
	config Config

	mu               sync.RWMutex
	state            State
	failures         int       // Failures in current window
	successes        int       // Successes in half-open state
	halfOpenInFlight int       // Probes admitted in half-open and not yet reported
	lastFailure      time.Time // For timeout calculation
	windowStart      time.Time // For failure window

	// Metrics (atomic for lock-free reads)
	totalRequests atomic.Int64
	totalFailures atomic.Int64
	totalSuccess  atomic.Int64
	totalRejected atomic.Int64
}

// New creates a new circuit breaker with the given configuration.
func New(config Config) *CircuitBreaker {
	if config.FailureThreshold == 0 {
		config.FailureThreshold = 5
	}
	if config.SuccessThreshold == 0 {
		config.SuccessThreshold = 1
	}
	if config.MaxHalfOpenRequests <= 0 {
		config.MaxHalfOpenRequests = config.SuccessThreshold
	}
	if config.Timeout == 0 {
		config.Timeout = 30 * time.Second
	}
	if config.Window == 0 {
		config.Window = 60 * time.Second
	}

	return &CircuitBreaker{
		config:      config,
		state:       StateClosed,
		windowStart: time.Now(),
	}
}

// Allow checks if a request should be allowed through.
//
// Returns nil if allowed, ErrCircuitOpen if blocked.
//
// Usage:
//
//	if err := cb.Allow(); err != nil {
//	    return err // Circuit is open
//	}
//	err := doRequest()
//	cb.RecordResult(err)
func (cb *CircuitBreaker) Allow() error {
	cb.mu.Lock()
	defer cb.mu.Unlock()

	cb.totalRequests.Add(1)

	switch cb.state {
	case StateClosed:
		// Always allow when closed
		return nil

	case StateOpen:
		// Check if timeout has elapsed
		if time.Since(cb.lastFailure) >= cb.config.Timeout {
			// Transition to half-open and admit this caller as the first probe.
			cb.state = StateHalfOpen
			cb.successes = 0
			cb.halfOpenInFlight = 1
			return nil
		}
		// Still in timeout period, reject
		cb.totalRejected.Add(1)
		return ErrCircuitOpen

	case StateHalfOpen:
		// Admission control: only MaxHalfOpenRequests probes may be in flight.
		// Without this the entire backlog reaches the recovering backend the
		// instant the timeout expires.
		if cb.halfOpenInFlight >= cb.config.MaxHalfOpenRequests {
			cb.totalRejected.Add(1)
			return ErrCircuitOpen
		}
		cb.halfOpenInFlight++
		return nil
	}

	return nil
}

// RecordResult records the outcome of a request.
//
// Call this after every request that was allowed through.
// Pass nil for success, error for failure.
func (cb *CircuitBreaker) RecordResult(err error) {
	cb.mu.Lock()
	defer cb.mu.Unlock()

	now := time.Now()

	if err == nil {
		// Success
		cb.totalSuccess.Add(1)

		switch cb.state {
		case StateHalfOpen:
			cb.releaseProbe()
			cb.successes++
			if cb.successes >= cb.config.SuccessThreshold {
				// Enough successes, close the circuit
				cb.state = StateClosed
				cb.failures = 0
				cb.successes = 0
				cb.halfOpenInFlight = 0
				cb.windowStart = now
			}
		case StateClosed:
			// Reset failure window on success
			cb.failures = 0
		}
	} else {
		// Failure
		cb.totalFailures.Add(1)

		switch cb.state {
		case StateHalfOpen:
			// Single failure in half-open reopens the circuit. Any sibling
			// probes still in flight are abandoned: the counter resets so the
			// next promotion to half-open starts from a clean slate.
			cb.state = StateOpen
			cb.lastFailure = now
			cb.successes = 0
			cb.halfOpenInFlight = 0

		case StateClosed:
			// Check if we need to start a new window
			if now.Sub(cb.windowStart) > cb.config.Window {
				cb.windowStart = now
				cb.failures = 0
			}
			cb.failures++
			cb.lastFailure = now

			if cb.failures >= cb.config.FailureThreshold {
				// Open the circuit
				cb.state = StateOpen
			}
		}
	}
}

// releaseProbe frees one half-open admission slot.
// Must be called with cb.mu held and cb.state == StateHalfOpen.
func (cb *CircuitBreaker) releaseProbe() {
	if cb.halfOpenInFlight > 0 {
		cb.halfOpenInFlight--
	}
}

// State returns the current circuit breaker state.
func (cb *CircuitBreaker) State() State {
	cb.mu.RLock()
	defer cb.mu.RUnlock()
	return cb.state
}

// Stats returns circuit breaker statistics.
type Stats struct {
	State         string
	TotalRequests int64
	TotalSuccess  int64
	TotalFailures int64
	TotalRejected int64
}

func (cb *CircuitBreaker) Stats() Stats {
	cb.mu.RLock()
	state := cb.state.String()
	cb.mu.RUnlock()

	return Stats{
		State:         state,
		TotalRequests: cb.totalRequests.Load(),
		TotalSuccess:  cb.totalSuccess.Load(),
		TotalFailures: cb.totalFailures.Load(),
		TotalRejected: cb.totalRejected.Load(),
	}
}

// Reset resets the circuit breaker to closed state.
// Useful for testing or manual intervention.
func (cb *CircuitBreaker) Reset() {
	cb.mu.Lock()
	defer cb.mu.Unlock()
	cb.state = StateClosed
	cb.failures = 0
	cb.successes = 0
	cb.halfOpenInFlight = 0
	cb.windowStart = time.Now()
}
