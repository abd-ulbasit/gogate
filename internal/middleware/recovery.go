package middleware

import (
	"log/slog"
	"net/http"
	"runtime/debug"
)

// Recovery returns a middleware that recovers from panics.
//
// Why recovery middleware?
// 1. Prevents single request panic from crashing the server
// 2. Logs panic with stack trace for debugging
// 3. Returns 500 error to client (graceful degradation)
//
// Placement: Should be early in the chain (close to handler)
// so it catches panics from the handler and later middlewares.
//
// Example chain: Logging → RateLimit → Recovery → Handler
// - If Handler panics, Recovery catches it
// - Logging still logs the 500 response
func Recovery(logger *slog.Logger) Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			defer func() {
				if err := recover(); err != nil {
					// Log panic with stack trace
					stack := debug.Stack()
					logger.Error("panic recovered",
						"error", err,
						"method", r.Method,
						"path", r.URL.Path,
						"stack", string(stack),
					)

					// Return 500 to client
					// Check if headers already sent
					// (can't write status if response already started)
					http.Error(w, "Internal Server Error", http.StatusInternalServerError)
				}
			}()

			next.ServeHTTP(w, r)
		})
	}
}
