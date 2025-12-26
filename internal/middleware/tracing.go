package middleware

import (
	"log/slog"
	"net/http"
	"time"

	"gogate/internal/observability"
	"gogate/internal/router"
)

// Tracing returns a middleware that adds request tracing.
//
// This middleware:
// 1. Creates a trace with unique request ID
// 2. Propagates X-Request-ID header (uses existing or generates new)
// 3. Adds trace to request context for downstream use
// 4. Logs request completion with timing
//
// Usage in middleware chain (should be early):
//
//	chain := middleware.Chain(
//	    middleware.Tracing(logger),
//	    middleware.Recovery(logger),
//	    middleware.Logging(logger),
//	    ...
//	)
func Tracing(logger *slog.Logger) Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// Create trace
			trace := observability.NewTrace("http.request")

			// Use existing X-Request-ID if present, otherwise use generated trace ID
			requestID := r.Header.Get("X-Request-ID")
			if requestID != "" {
				trace.ID = requestID
			} else {
				r.Header.Set("X-Request-ID", trace.ID)
			}

			// Set response header for client correlation
			w.Header().Set("X-Request-ID", trace.ID)

			// Add trace attributes from request
			trace.SetAttr("method", r.Method)
			trace.SetAttr("path", r.URL.Path)
			trace.SetAttr("remote_addr", r.RemoteAddr)

			// Add to context
			ctx := observability.WithTrace(r.Context(), trace)

			// Also add enriched logger to context
			enrichedLogger := observability.LoggerWithTrace(logger, trace)
			ctx = observability.WithLogger(ctx, enrichedLogger)

			// Wrap response writer to capture status
			wrapped := wrapResponseWriter(w)

			// Process request
			next.ServeHTTP(wrapped, r.WithContext(ctx))

			// End trace and record
			trace.End()

			// Get route info if available
			if route := router.RouteFromContext(ctx); route != nil {
				trace.SetAttr("route", route.Name)
			}

			// Log completion
			enrichedLogger.Info("request.complete",
				"status", wrapped.status,
				"bytes", wrapped.bytes,
				"duration_ms", float64(trace.Duration().Microseconds())/1000,
			)
		})
	}
}

// TracingWithRequestID is a simpler version that only handles request ID.
//
// Use this when you want request ID correlation without full tracing.
// Lighter weight than full Tracing middleware.
func TracingWithRequestID() Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			requestID := r.Header.Get("X-Request-ID")
			if requestID == "" {
				requestID = observability.RequestID()
				r.Header.Set("X-Request-ID", requestID)
			}

			// Set response header
			w.Header().Set("X-Request-ID", requestID)

			// Add to context
			ctx := observability.WithRequestID(r.Context(), requestID)

			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// LoggingWithTrace returns enhanced logging middleware that uses trace context.
//
// This is an alternative to the basic Logging middleware that integrates
// with the tracing system for richer logs.
//
// Should be used AFTER Tracing middleware in the chain:
//
//	chain := middleware.Chain(
//	    middleware.Tracing(logger),
//	    middleware.LoggingWithTrace(logger),
//	    ...
//	)
func LoggingWithTrace(logger *slog.Logger) Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()
			wrapped := wrapResponseWriter(w)

			next.ServeHTTP(wrapped, r)

			duration := time.Since(start)

			// Get trace from context for enriched logging
			trace := observability.TraceFromContext(r.Context())
			l := observability.LoggerWithTrace(logger, trace)

			// Get route info
			routeName := ""
			if route := router.RouteFromContext(r.Context()); route != nil {
				routeName = route.Name
			}

			l.Info("http.request",
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
