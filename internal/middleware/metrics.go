package middleware

import (
	"net/http"
	"time"

	"gogate/internal/metrics"
	"gogate/internal/ratelimiter"
)

// Metrics returns a middleware that records HTTP request metrics.
//
// Metrics recorded:
// - Request count (total, success, failure by status code range)
// - Request duration histogram
// - Response size
//
// Works with metrics.Collector which can be exported via Prometheus.
func Metrics(collector *metrics.Collector) Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()
			wrapped := wrapResponseWriter(w)

			// Track connection
			collector.RecordConnection()

			// Process request
			next.ServeHTTP(wrapped, r)

			// Record metrics
			duration := time.Since(start)
			success := wrapped.status >= 200 && wrapped.status < 400

			// Use path as "backend" for HTTP metrics grouping
			// In production, you might use route name instead
			backend := r.URL.Path
			if len(backend) > 50 {
				backend = backend[:50] // Truncate very long paths
			}

			collector.RecordRequest(backend, success, duration, r.ContentLength, wrapped.bytes)
			collector.RecordConnectionClosed(duration)
		})
	}
}

// RateLimitWithMetrics wraps the RateLimit middleware to record metrics.
//
// Records allowed and rejected request counts for Prometheus.
func RateLimitWithMetrics(rl *ratelimiter.TokenBucket, stats *metrics.RateLimiterWithStats) Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if !rl.Allow() {
				stats.RecordRejected()
				http.Error(w, "rate limit exceeded", http.StatusTooManyRequests)
				return
			}
			stats.RecordAllowed()
			next.ServeHTTP(w, r)
		})
	}
}
