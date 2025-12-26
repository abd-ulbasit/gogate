package middleware

import (
	"log/slog"
	"net/http"
	"time"

	"gogate/internal/router"
)

// responseWriter wraps http.ResponseWriter to capture status code and bytes.
//
// Why wrap?
// - http.ResponseWriter doesn't expose status code after WriteHeader()
// - Need to capture response size for logging/metrics
// - Standard pattern used by most logging middleware
type responseWriter struct {
	http.ResponseWriter
	status      int
	wroteHeader bool
	bytes       int64
}

func wrapResponseWriter(w http.ResponseWriter) *responseWriter {
	return &responseWriter{ResponseWriter: w, status: http.StatusOK}
}

func (rw *responseWriter) WriteHeader(code int) {
	if !rw.wroteHeader {
		rw.status = code
		rw.wroteHeader = true
		rw.ResponseWriter.WriteHeader(code)
	}
}

func (rw *responseWriter) Write(b []byte) (int, error) {
	if !rw.wroteHeader {
		rw.WriteHeader(http.StatusOK)
	}
	n, err := rw.ResponseWriter.Write(b)
	rw.bytes += int64(n)
	return n, err
}

// Unwrap returns the original ResponseWriter.
// Required for http.Flusher, http.Hijacker compatibility.
func (rw *responseWriter) Unwrap() http.ResponseWriter {
	return rw.ResponseWriter
}

// Logging returns a middleware that logs HTTP requests.
//
// Log format (structured JSON via slog):
//
//	{
//	    "level": "INFO",
//	    "msg": "http request",
//	    "method": "GET",
//	    "path": "/api/users",
//	    "status": 200,
//	    "duration_ms": 12.5,
//	    "bytes": 1234,
//	    "route": "api-route",
//	    "client_ip": "192.168.1.1"
//	}
func Logging(logger *slog.Logger) Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()

			// Wrap response writer to capture status and bytes
			wrapped := wrapResponseWriter(w)

			// Process request
			next.ServeHTTP(wrapped, r)

			// Calculate duration
			duration := time.Since(start)

			// Get route info from context (if available)
			routeName := ""
			if route := router.RouteFromContext(r.Context()); route != nil {
				routeName = route.Name
			}

			// Log request
			logger.Info("http request",
				"method", r.Method,
				"path", r.URL.Path,
				"query", r.URL.RawQuery,
				"status", wrapped.status,
				"duration_ms", float64(duration.Microseconds())/1000,
				"bytes", wrapped.bytes,
				"route", routeName,
				"client_ip", clientIP(r),
				"user_agent", r.UserAgent(),
			)
		})
	}
}

// clientIP extracts the client IP from the request.
//
// Priority:
// 1. X-Forwarded-For header (first IP in list)
// 2. X-Real-IP header
// 3. RemoteAddr
func clientIP(r *http.Request) string {
	// Check X-Forwarded-For (may contain multiple IPs)
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		// Take first IP (original client)
		for i := 0; i < len(xff); i++ {
			if xff[i] == ',' {
				return xff[:i]
			}
		}
		return xff
	}

	// Check X-Real-IP
	if xri := r.Header.Get("X-Real-IP"); xri != "" {
		return xri
	}

	// Fall back to RemoteAddr
	return r.RemoteAddr
}
