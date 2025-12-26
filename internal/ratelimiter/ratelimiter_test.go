package ratelimiter

import (
	"testing"
	"time"
)

func TestTokenBucketAllowsUpToBurst(t *testing.T) {
	// Rate: 100/sec, burst: 5
	tb := NewTokenBucket(100, 5)

	// Should allow burst of 5 immediately
	for i := 0; i < 5; i++ {
		if !tb.Allow() {
			t.Errorf("request %d should be allowed", i)
		}
	}

	// 6th request should be denied (bucket empty)
	if tb.Allow() {
		t.Error("request 6 should be denied (bucket empty)")
	}
}

func TestTokenBucketRefillsOverTime(t *testing.T) {
	// Rate: 100/sec, burst: 5
	tb := NewTokenBucket(100, 5)

	// Drain the bucket
	for i := 0; i < 5; i++ {
		tb.Allow()
	}

	// Should be denied now
	if tb.Allow() {
		t.Error("should be denied when empty")
	}

	// Wait for refill (10ms = 1 token at 100/sec)
	time.Sleep(15 * time.Millisecond)

	// Should allow 1 request now
	if !tb.Allow() {
		t.Error("should be allowed after refill")
	}
}

func TestTokenBucketDoesNotExceedCapacity(t *testing.T) {
	// Rate: 100/sec, burst: 3
	tb := NewTokenBucket(100, 3)

	// Use 1 token
	tb.Allow()

	// Wait long enough to refill way more than capacity
	time.Sleep(100 * time.Millisecond)

	// Should still only have 3 tokens (capacity)
	stats := tb.Stats()
	if stats.Tokens > 3 {
		t.Errorf("tokens should not exceed capacity, got %.2f", stats.Tokens)
	}
}

func TestTokenBucketAllowN(t *testing.T) {
	// Rate: 100/sec, burst: 10
	tb := NewTokenBucket(100, 10)

	// Request 5 tokens
	if !tb.AllowN(5) {
		t.Error("AllowN(5) should succeed")
	}

	// Request 5 more (should have exactly 5 left)
	if !tb.AllowN(5) {
		t.Error("second AllowN(5) should succeed")
	}

	// Request 1 more (should fail, bucket empty)
	if tb.AllowN(1) {
		t.Error("AllowN(1) should fail when empty")
	}
}

func TestTokenBucketStats(t *testing.T) {
	tb := NewTokenBucket(50, 5)

	// Allow 3
	for i := 0; i < 3; i++ {
		tb.Allow()
	}

	// Deny 2 (drain remaining 2, then 0 left)
	tb.Allow() // 4th - allowed
	tb.Allow() // 5th - allowed
	tb.Allow() // 6th - denied

	stats := tb.Stats()
	if stats.Rate != 50 {
		t.Errorf("expected rate 50, got %.0f", stats.Rate)
	}
	if stats.Capacity != 5 {
		t.Errorf("expected capacity 5, got %.0f", stats.Capacity)
	}
	if stats.TotalAllowed != 5 {
		t.Errorf("expected 5 allowed, got %d", stats.TotalAllowed)
	}
	if stats.TotalDenied != 1 {
		t.Errorf("expected 1 denied, got %d", stats.TotalDenied)
	}
}

func TestTokenBucketSetRate(t *testing.T) {
	tb := NewTokenBucket(10, 5)

	// Drain bucket
	for i := 0; i < 5; i++ {
		tb.Allow()
	}

	// Increase rate to 1000/sec
	tb.SetRate(1000)

	// Wait 10ms = 10 tokens at 1000/sec (capped at 5)
	time.Sleep(10 * time.Millisecond)

	// Should allow 5 requests
	for i := 0; i < 5; i++ {
		if !tb.Allow() {
			t.Errorf("request %d should be allowed after rate increase", i)
		}
	}
}

func TestTokenBucketReset(t *testing.T) {
	tb := NewTokenBucket(100, 10)

	// Drain bucket
	for i := 0; i < 10; i++ {
		tb.Allow()
	}

	// Should be denied
	if tb.Allow() {
		t.Error("should be denied when empty")
	}

	// Reset
	tb.Reset()

	// Should have full capacity again
	for i := 0; i < 10; i++ {
		if !tb.Allow() {
			t.Errorf("request %d should be allowed after reset", i)
		}
	}
}

func TestTokenBucketConcurrent(t *testing.T) {
	// Test thread safety
	tb := NewTokenBucket(1000, 100)

	done := make(chan bool)
	for i := 0; i < 10; i++ {
		go func() {
			for j := 0; j < 100; j++ {
				tb.Allow()
			}
			done <- true
		}()
	}

	for i := 0; i < 10; i++ {
		<-done
	}

	// Just checking it doesn't panic/deadlock
	stats := tb.Stats()
	total := stats.TotalAllowed + stats.TotalDenied
	if total != 1000 {
		t.Errorf("expected 1000 total, got %d", total)
	}
}
