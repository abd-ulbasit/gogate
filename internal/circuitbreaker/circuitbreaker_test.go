package circuitbreaker

import (
	"errors"
	"testing"
	"time"
)

func TestCircuitBreakerStartsClosed(t *testing.T) {
	cb := New(DefaultConfig())
	if cb.State() != StateClosed {
		t.Errorf("expected state CLOSED, got %s", cb.State())
	}
}

func TestCircuitBreakerAllowsWhenClosed(t *testing.T) {
	cb := New(DefaultConfig())

	// Should allow requests when closed
	for i := 0; i < 10; i++ {
		if err := cb.Allow(); err != nil {
			t.Errorf("request %d: expected Allow() to succeed, got %v", i, err)
		}
		cb.RecordResult(nil) // success
	}
}

func TestCircuitBreakerOpensAfterFailures(t *testing.T) {
	cfg := Config{
		FailureThreshold: 3,
		Timeout:          1 * time.Second,
	}
	cb := New(cfg)

	// Fail 3 times
	for i := 0; i < 3; i++ {
		cb.Allow()
		cb.RecordResult(errors.New("backend error"))
	}

	// Should be open now
	if cb.State() != StateOpen {
		t.Errorf("expected state OPEN after %d failures, got %s", cfg.FailureThreshold, cb.State())
	}

	// Should reject requests
	if err := cb.Allow(); err != ErrCircuitOpen {
		t.Errorf("expected ErrCircuitOpen, got %v", err)
	}
}

func TestCircuitBreakerTransitionsToHalfOpen(t *testing.T) {
	cfg := Config{
		FailureThreshold: 1,
		Timeout:          50 * time.Millisecond,
	}
	cb := New(cfg)

	// Open the circuit
	cb.Allow()
	cb.RecordResult(errors.New("fail"))

	if cb.State() != StateOpen {
		t.Fatalf("expected OPEN, got %s", cb.State())
	}

	// Wait for timeout
	time.Sleep(100 * time.Millisecond)

	// Next Allow() should transition to half-open
	if err := cb.Allow(); err != nil {
		t.Errorf("expected Allow() to succeed in half-open, got %v", err)
	}

	if cb.State() != StateHalfOpen {
		t.Errorf("expected HALF-OPEN after timeout, got %s", cb.State())
	}
}

func TestCircuitBreakerClosesAfterSuccessInHalfOpen(t *testing.T) {
	cfg := Config{
		FailureThreshold: 1,
		SuccessThreshold: 1,
		Timeout:          50 * time.Millisecond,
	}
	cb := New(cfg)

	// Open the circuit
	cb.Allow()
	cb.RecordResult(errors.New("fail"))

	// Wait for half-open
	time.Sleep(100 * time.Millisecond)
	cb.Allow() // Transitions to half-open

	// Record success
	cb.RecordResult(nil)

	if cb.State() != StateClosed {
		t.Errorf("expected CLOSED after success in half-open, got %s", cb.State())
	}
}

func TestCircuitBreakerReopensAfterFailureInHalfOpen(t *testing.T) {
	cfg := Config{
		FailureThreshold: 1,
		Timeout:          50 * time.Millisecond,
	}
	cb := New(cfg)

	// Open the circuit
	cb.Allow()
	cb.RecordResult(errors.New("fail"))

	// Wait for half-open
	time.Sleep(100 * time.Millisecond)
	cb.Allow() // Transitions to half-open

	// Record another failure
	cb.RecordResult(errors.New("still failing"))

	if cb.State() != StateOpen {
		t.Errorf("expected OPEN after failure in half-open, got %s", cb.State())
	}
}

func TestCircuitBreakerStats(t *testing.T) {
	cb := New(DefaultConfig())

	// Some successes
	for i := 0; i < 5; i++ {
		cb.Allow()
		cb.RecordResult(nil)
	}

	// Some failures (not enough to open)
	for i := 0; i < 2; i++ {
		cb.Allow()
		cb.RecordResult(errors.New("error"))
	}

	stats := cb.Stats()
	if stats.TotalRequests != 7 {
		t.Errorf("expected 7 total requests, got %d", stats.TotalRequests)
	}
	if stats.TotalSuccess != 5 {
		t.Errorf("expected 5 successes, got %d", stats.TotalSuccess)
	}
	if stats.TotalFailures != 2 {
		t.Errorf("expected 2 failures, got %d", stats.TotalFailures)
	}
}

func TestCircuitBreakerReset(t *testing.T) {
	cfg := Config{FailureThreshold: 1}
	cb := New(cfg)

	// Open the circuit
	cb.Allow()
	cb.RecordResult(errors.New("fail"))

	if cb.State() != StateOpen {
		t.Fatalf("expected OPEN, got %s", cb.State())
	}

	// Reset
	cb.Reset()

	if cb.State() != StateClosed {
		t.Errorf("expected CLOSED after reset, got %s", cb.State())
	}

	// Should allow requests again
	if err := cb.Allow(); err != nil {
		t.Errorf("expected Allow() to succeed after reset, got %v", err)
	}
}
