package middleware

import (
	"net/http"
	"strconv"
	"strings"
)

// CORSConfig configures Cross-Origin Resource Sharing (CORS) behavior.
//
// CORS is a security feature that restricts web pages from making requests
// to a different domain than the one serving the page.
//
// When is CORS needed?
//   - Frontend at https://app.example.com calls API at https://api.example.com
//   - Browser blocks request unless API returns proper CORS headers
//
// CORS flow:
//  1. Browser sends OPTIONS preflight request (for non-simple requests)
//  2. Server responds with allowed origins/methods/headers
//  3. Browser decides whether to allow the actual request
//  4. Actual request includes Origin header
//  5. Server includes CORS headers in response
type CORSConfig struct {
	// AllowedOrigins is a list of origins that are allowed to access the resource.
	// Use "*" to allow any origin (NOT recommended for production with credentials).
	// Example: ["https://app.example.com", "https://admin.example.com"]
	AllowedOrigins []string

	// AllowedMethods is a list of HTTP methods allowed for CORS requests.
	// Example: ["GET", "POST", "PUT", "DELETE", "OPTIONS"]
	AllowedMethods []string

	// AllowedHeaders is a list of headers that can be used in the actual request.
	// Example: ["Authorization", "Content-Type", "X-Request-ID"]
	AllowedHeaders []string

	// ExposedHeaders is a list of headers that browsers are allowed to access.
	// By default, browsers can only access simple headers (Cache-Control, etc.)
	// Example: ["X-Request-ID", "X-RateLimit-Remaining"]
	ExposedHeaders []string

	// AllowCredentials indicates whether cookies/auth headers can be included.
	// If true, AllowedOrigins cannot be "*" (browser requirement).
	AllowCredentials bool

	// MaxAge indicates how long (in seconds) browsers can cache preflight results.
	// Higher values reduce preflight requests but delay CORS policy updates.
	// Default: 0 (no caching), Recommended: 86400 (24 hours)
	MaxAge int
}

// DefaultCORSConfig returns a permissive CORS configuration for development.
//
// WARNING: Do not use in production! This allows any origin.
// In production, explicitly list allowed origins.
func DefaultCORSConfig() CORSConfig {
	return CORSConfig{
		AllowedOrigins: []string{"*"},
		AllowedMethods: []string{"GET", "POST", "PUT", "DELETE", "PATCH", "OPTIONS"},
		AllowedHeaders: []string{"Authorization", "Content-Type", "X-Request-ID"},
		MaxAge:         86400, // 24 hours
	}
}

// CORS returns a middleware that handles Cross-Origin Resource Sharing.
//
// The middleware:
//  1. Handles OPTIONS preflight requests (returns 204 with CORS headers)
//  2. Adds CORS headers to all responses
//  3. Validates origin against allowed list
//
// Preflight requests:
//   - Browser sends OPTIONS before "non-simple" requests
//   - Non-simple = custom headers, methods other than GET/POST, JSON content-type
//   - Server must respond with CORS headers to allow the actual request
//
// Example:
//
//	corsMiddleware := middleware.CORS(middleware.CORSConfig{
//	    AllowedOrigins: []string{"https://app.example.com"},
//	    AllowedMethods: []string{"GET", "POST"},
//	    AllowCredentials: true,
//	})
func CORS(config CORSConfig) Middleware {
	// Pre-compute header values for performance
	allowedMethodsStr := strings.Join(config.AllowedMethods, ", ")
	allowedHeadersStr := strings.Join(config.AllowedHeaders, ", ")
	exposedHeadersStr := strings.Join(config.ExposedHeaders, ", ")
	maxAgeStr := strconv.Itoa(config.MaxAge)

	// Build origin lookup map for O(1) checks
	originAllowed := make(map[string]bool)
	allowAnyOrigin := false
	for _, origin := range config.AllowedOrigins {
		if origin == "*" {
			allowAnyOrigin = true
		} else {
			originAllowed[origin] = true
		}
	}

	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			origin := r.Header.Get("Origin")

			// No Origin header = same-origin request, no CORS needed
			if origin == "" {
				next.ServeHTTP(w, r)
				return
			}

			// Check if origin is allowed
			allowed := allowAnyOrigin || originAllowed[origin]
			if !allowed {
				// Origin not allowed - don't add CORS headers
				// Browser will block the response
				next.ServeHTTP(w, r)
				return
			}

			// Set CORS headers
			// Why set specific origin instead of "*"?
			// - If AllowCredentials is true, "*" is not allowed
			// - Setting specific origin is more secure anyway
			if allowAnyOrigin && !config.AllowCredentials {
				w.Header().Set("Access-Control-Allow-Origin", "*")
			} else {
				w.Header().Set("Access-Control-Allow-Origin", origin)
				// Vary header tells caches that response varies by Origin
				w.Header().Add("Vary", "Origin")
			}

			if config.AllowCredentials {
				w.Header().Set("Access-Control-Allow-Credentials", "true")
			}

			if exposedHeadersStr != "" {
				w.Header().Set("Access-Control-Expose-Headers", exposedHeadersStr)
			}

			// Handle preflight request (OPTIONS method)
			if r.Method == http.MethodOptions {
				w.Header().Set("Access-Control-Allow-Methods", allowedMethodsStr)
				w.Header().Set("Access-Control-Allow-Headers", allowedHeadersStr)
				if config.MaxAge > 0 {
					w.Header().Set("Access-Control-Max-Age", maxAgeStr)
				}
				// Return 204 No Content for preflight
				w.WriteHeader(http.StatusNoContent)
				return
			}

			next.ServeHTTP(w, r)
		})
	}
}
