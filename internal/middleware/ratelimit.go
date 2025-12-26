package middleware

import (
	"net/http"

	"gogate/internal/ratelimiter"
)

// RateLimit returns a middleware that rate limits HTTP requests.
//
// Uses the existing TokenBucket rate limiter from internal/ratelimiter.
//
// Returns 429 Too Many Requests when rate limit exceeded.
// Includes Retry-After header with suggested wait time.
func RateLimit(limiter *ratelimiter.TokenBucket) Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if !limiter.Allow() {
				// Set Retry-After header (1 second is a reasonable default)
				w.Header().Set("Retry-After", "1")
				http.Error(w, "Too Many Requests", http.StatusTooManyRequests)
				return
			}

			next.ServeHTTP(w, r)
		})
	}
}

// RateLimitByIP returns a middleware with per-IP rate limiting.
//
// TODO(basit): Implement per-IP rate limiting
// This requires a map of IP → TokenBucket with cleanup for stale entries.
// Consider using sync.Map or sharded map for high concurrency.
//
// Design considerations:
// - Memory: Need to bound number of tracked IPs
// - Cleanup: Remove stale entries to prevent memory leak
// - Fairness: Consider using sliding window for smoother limiting
// func RateLimitByIP(rate float64, burst int) Middleware {
//     // Your implementation here
// }
