package middleware

import (
	"net/http"

	"github.com/abd-ulbasit/sluice/internal/ratelimiter"
)

// RateLimit returns a middleware that rate limits HTTP requests against one
// shared TokenBucket from internal/ratelimiter.
//
// The bucket has no client dimension. Every request draws from the same
// budget, whoever sent it, so this is a ceiling on what the backend pool is
// asked to absorb — not a fairness mechanism. A single client sending at the
// configured rate consumes the whole budget and every other client sees 429;
// nothing here can tell the two cases apart.
//
// Per-IP buckets are absent by decision, not by omission. Keying a bucket by
// remote address needs a bound on how many are tracked and an eviction path
// for stale ones, and without both, a proxy that accepts untrusted
// connections has handed out a memory-growth primitive costing one source
// address per entry. Shipping that in front of the traffic this proxy is
// built for would be a worse bug than the gap it closes. The gap is stated in
// the README under "Limitations" and in docs/DESIGN-DECISIONS.md.
//
// Returns 429 Too Many Requests, with a Retry-After header, when the bucket
// is empty.
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
