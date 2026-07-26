package circuitbreaker

import (
	"errors"
	"sync"
	"sync/atomic"
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

// TestHalfOpenAdmitsAtMostMaxProbes is the regression test for the thundering-herd
// bug: when the open-state timeout elapsed, every goroutine blocked behind the
// breaker was admitted at once, so a backend that had just been declared dead
// received the full concurrent load as its first traffic after recovery.
//
// The breaker must admit at most MaxHalfOpenRequests probes and reject the rest
// with ErrCircuitOpen until one of the probes reports a result.
func TestHalfOpenAdmitsAtMostMaxProbes(t *testing.T) {
	const concurrency = 64
	const maxProbes = 2

	cb := New(Config{
		FailureThreshold:    1,
		SuccessThreshold:    5, // deliberately > maxProbes so the circuit stays half-open
		MaxHalfOpenRequests: maxProbes,
		Timeout:             20 * time.Millisecond,
	})

	// Trip the circuit.
	cb.Allow()
	cb.RecordResult(errors.New("backend down"))
	if cb.State() != StateOpen {
		t.Fatalf("expected OPEN after failure, got %s", cb.State())
	}

	// Wait out the open-state timeout so the next Allow() promotes to half-open.
	time.Sleep(40 * time.Millisecond)

	// Release every goroutine at the same instant.
	var (
		start    = make(chan struct{})
		wg       sync.WaitGroup
		admitted atomic.Int64
	)
	wg.Add(concurrency)
	for i := 0; i < concurrency; i++ {
		go func() {
			defer wg.Done()
			<-start
			if err := cb.Allow(); err == nil {
				admitted.Add(1)
			}
		}()
	}
	close(start)
	wg.Wait()

	if got := admitted.Load(); got != maxProbes {
		t.Errorf("half-open admitted %d of %d concurrent requests, want exactly %d",
			got, concurrency, maxProbes)
	}
	if cb.State() != StateHalfOpen {
		t.Errorf("expected state HALF-OPEN, got %s", cb.State())
	}
}

// TestHalfOpenReleasesProbeSlotOnResult verifies that a completed probe frees its
// slot, so a stuck breaker cannot permanently reject traffic after the backend
// recovers.
func TestHalfOpenReleasesProbeSlotOnResult(t *testing.T) {
	cb := New(Config{
		FailureThreshold:    1,
		SuccessThreshold:    3,
		MaxHalfOpenRequests: 1,
		Timeout:             20 * time.Millisecond,
	})

	cb.Allow()
	cb.RecordResult(errors.New("backend down"))
	time.Sleep(40 * time.Millisecond)

	// Probe 1 is admitted by the OPEN -> HALF-OPEN promotion.
	if err := cb.Allow(); err != nil {
		t.Fatalf("first probe: expected admission, got %v", err)
	}
	// Probe 2 must be rejected: probe 1 has not reported yet.
	if err := cb.Allow(); !errors.Is(err, ErrCircuitOpen) {
		t.Fatalf("second probe: expected ErrCircuitOpen while a probe is in flight, got %v", err)
	}
	// Probe 1 reports success; state is still half-open (SuccessThreshold is 3).
	cb.RecordResult(nil)
	if cb.State() != StateHalfOpen {
		t.Fatalf("expected HALF-OPEN, got %s", cb.State())
	}
	// The slot is free again.
	if err := cb.Allow(); err != nil {
		t.Fatalf("third probe: expected admission after slot released, got %v", err)
	}
}

// TestHalfOpenDefaultsToSuccessThreshold documents the default: the breaker admits
// exactly as many probes as it needs successes to close, so the happy path takes
// one round trip rather than SuccessThreshold sequential round trips.
func TestHalfOpenDefaultsToSuccessThreshold(t *testing.T) {
	cb := New(Config{
		FailureThreshold: 1,
		SuccessThreshold: 3,
		Timeout:          20 * time.Millisecond,
	})
	if cb.config.MaxHalfOpenRequests != 3 {
		t.Fatalf("MaxHalfOpenRequests defaulted to %d, want SuccessThreshold (3)",
			cb.config.MaxHalfOpenRequests)
	}

	cb.Allow()
	cb.RecordResult(errors.New("backend down"))
	time.Sleep(40 * time.Millisecond)

	for i := 0; i < 3; i++ {
		if err := cb.Allow(); err != nil {
			t.Fatalf("probe %d: expected admission, got %v", i, err)
		}
	}
	if err := cb.Allow(); !errors.Is(err, ErrCircuitOpen) {
		t.Fatalf("probe 4: expected ErrCircuitOpen, got %v", err)
	}
}
