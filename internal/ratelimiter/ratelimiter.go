package ratelimiter

import (
	"sync"
	"time"
)

// RateLimiter controls the rate of requests.
//
// Two common algorithms:
//
// 1. TOKEN BUCKET (this implementation):
//    - Bucket holds tokens (capacity = burst limit)
//    - Tokens added at fixed rate (e.g., 100/sec)
//    - Each request consumes one token
//    - If no tokens, request is rejected or waits
//    - Allows bursts up to bucket capacity
//
// 2. SLIDING WINDOW (alternative):
//    - Track requests in recent time window
//    - Smoother rate limiting, no bursts
//    - More memory (need to track timestamps)
//
// Why token bucket?
// - Simpler to implement
// - Allows controlled bursts
// - Good for most use cases
//
// Usage:
//   limiter := NewTokenBucket(100, 10) // 100 req/sec, burst of 10
//   if limiter.Allow() {
//       handleRequest()
//   } else {
//       return 429 Too Many Requests
//   }

// TokenBucket implements token bucket rate limiting.
//
// Thread-safe: All methods are safe for concurrent use.
type TokenBucket struct {
	mu sync.Mutex

	// Configuration
	rate     float64 // Tokens per second
	capacity float64 // Max tokens (burst size)

	// State
	tokens   float64   // Current tokens
	lastFill time.Time // When tokens were last added

	// Metrics
	totalAllowed int64
	totalDenied  int64
}

// NewTokenBucket creates a new token bucket rate limiter.
//
// Parameters:
// - rate: tokens per second (requests allowed per second)
// - burst: maximum burst size (tokens that can accumulate)
//
// Example:
//
//	NewTokenBucket(100, 10) // 100 req/sec, can burst 10 requests
func NewTokenBucket(rate float64, burst int) *TokenBucket {
	if rate <= 0 {
		rate = 1
	}
	if burst <= 0 {
		burst = 1
	}

	return &TokenBucket{
		rate:     rate,
		capacity: float64(burst),
		tokens:   float64(burst), // Start with full bucket
		lastFill: time.Now(),
	}
}

// Allow checks if a request should be allowed.
//
// Returns true if allowed (token consumed), false if rate limited.
// Non-blocking - returns immediately.
func (tb *TokenBucket) Allow() bool {
	return tb.AllowN(1)
}

// AllowN checks if n tokens are available.
//
// Useful for requests with different "costs" (e.g., batch operations).
func (tb *TokenBucket) AllowN(n int) bool {
	tb.mu.Lock()
	defer tb.mu.Unlock()

	// Refill tokens based on time elapsed
	tb.refill()

	// Check if enough tokens available
	if tb.tokens >= float64(n) {
		tb.tokens -= float64(n)
		tb.totalAllowed++
		return true
	}

	tb.totalDenied++
	return false
}

// Wait blocks until a token is available or context is cancelled.
//
// Use when you want to queue requests instead of rejecting.
// func (tb *TokenBucket) Wait(ctx context.Context) error {
//     // TODO: Implement waiting with context cancellation
// }

// Stats returns rate limiter statistics.
type Stats struct {
	Rate         float64 // Configured rate
	Capacity     float64 // Configured capacity
	Tokens       float64 // Current tokens
	TotalAllowed int64
	TotalDenied  int64
}

func (tb *TokenBucket) Stats() Stats {
	tb.mu.Lock()
	defer tb.mu.Unlock()

	// Refill before returning stats
	tb.refill()

	return Stats{
		Rate:         tb.rate,
		Capacity:     tb.capacity,
		Tokens:       tb.tokens,
		TotalAllowed: tb.totalAllowed,
		TotalDenied:  tb.totalDenied,
	}
}

// refill adds tokens based on elapsed time.
// Must be called with lock held.
func (tb *TokenBucket) refill() {
	now := time.Now()
	elapsed := now.Sub(tb.lastFill).Seconds()
	tb.tokens += tb.rate * elapsed
	if tb.tokens > tb.capacity {
		tb.tokens = tb.capacity
	}
	tb.lastFill = now
}

// SetRate changes the rate limit dynamically.
func (tb *TokenBucket) SetRate(rate float64) {
	if rate <= 0 {
		return
	}
	tb.mu.Lock()
	defer tb.mu.Unlock()
	tb.refill() // Apply any accumulated tokens first
	tb.rate = rate
}

// Reset resets the limiter to full capacity.
func (tb *TokenBucket) Reset() {
	tb.mu.Lock()
	defer tb.mu.Unlock()
	tb.tokens = tb.capacity
	tb.lastFill = time.Now()
}
