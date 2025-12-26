package middleware

import (
	"net"
	"net/http"
	"strings"
)

// Headers returns a middleware that sets proxy headers.
//
// Headers set:
// - X-Forwarded-For: Client IP (appends to existing if present)
// - X-Forwarded-Proto: http or https
// - X-Forwarded-Host: Original Host header
// - X-Real-IP: Client IP (only if not already set)
//
// Why these headers?
// - Backend needs to know original client IP for logging/rate limiting
// - Backend needs to know original protocol for URL generation
// - Standard headers used by nginx, HAProxy, etc.
func Headers() Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// Get client IP
			clientIP := extractClientIP(r)

			// X-Forwarded-For: Append to existing (proxy chain)
			if prior := r.Header.Get("X-Forwarded-For"); prior != "" {
				clientIP = prior + ", " + clientIP
			}
			r.Header.Set("X-Forwarded-For", clientIP)

			// X-Real-IP: Set only if not already present
			if r.Header.Get("X-Real-IP") == "" {
				r.Header.Set("X-Real-IP", extractClientIP(r))
			}

			// X-Forwarded-Host: Original Host header
			if r.Header.Get("X-Forwarded-Host") == "" {
				r.Header.Set("X-Forwarded-Host", r.Host)
			}

			// X-Forwarded-Proto: http or https
			proto := "http"
			if r.TLS != nil {
				proto = "https"
			}
			if r.Header.Get("X-Forwarded-Proto") == "" {
				r.Header.Set("X-Forwarded-Proto", proto)
			}

			next.ServeHTTP(w, r)
		})
	}
}

// extractClientIP extracts the client IP from RemoteAddr.
//
// RemoteAddr is in "IP:port" format, so we need to strip the port.
func extractClientIP(r *http.Request) string {
	// RemoteAddr is "IP:port"
	ip, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		// Fallback: return as-is (might be IP without port)
		return r.RemoteAddr
	}
	return ip
}

// RequestID returns a middleware that ensures X-Request-ID header exists.
//
// If the incoming request has X-Request-ID, it's preserved.
// Otherwise, a new UUID is generated.
//
// Why request IDs?
// - Correlate logs across services (distributed tracing lite)
// - Debug specific requests in production
// - Track request flow through proxy → backend
func RequestID(generator func() string) Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			requestID := r.Header.Get("X-Request-ID")
			if requestID == "" {
				requestID = generator()
				r.Header.Set("X-Request-ID", requestID)
			}

			// Also set in response for client correlation
			w.Header().Set("X-Request-ID", requestID)

			next.ServeHTTP(w, r)
		})
	}
}

// StripPrefix returns a middleware that strips a path prefix before forwarding.
//
// Example:
//
//	Route: /api/v1/* → backend
//	StripPrefix("/api/v1") makes /api/v1/users → /users
//
// Useful when backend doesn't know about proxy's path structure.
func StripPrefix(prefix string) Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if strings.HasPrefix(r.URL.Path, prefix) {
				r.URL.Path = strings.TrimPrefix(r.URL.Path, prefix)
				if r.URL.Path == "" {
					r.URL.Path = "/"
				}
			}
			next.ServeHTTP(w, r)
		})
	}
}
