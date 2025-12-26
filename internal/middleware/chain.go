package middleware

import "net/http"

// Middleware is the standard Go middleware signature.
//
// Why this pattern?
// 1. Composable: Middlewares can be chained together
// 2. Standard: Used by chi, gorilla, stdlib
// 3. Simple: No framework-specific types
//
// How it works:
//
//	middleware := func(next http.Handler) http.Handler {
//	    return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
//	        // Before logic
//	        next.ServeHTTP(w, r)  // Call next handler
//	        // After logic
//	    })
//	}
type Middleware func(http.Handler) http.Handler

// Chain wraps a handler with multiple middlewares.
//
// Execution order: First middleware in the list executes first (outermost).
//
// RECOMMENDED ORDER for API gateway:
//
//	Chain(handler,
//	    Recovery,   // 1st: Catch panics from ALL middleware
//	    Logging,    // 2nd: Log ALL requests (including rate-limited)
//	    RateLimit,  // 3rd: Block excess traffic (logged above)
//	    Headers,    // 4th: Set proxy headers
//	    Auth,       // 5th: Validate JWT/auth
//	)
//
// Why this order?
//   - Recovery outermost: Catches panics from any middleware
//   - Logging before RateLimit: Rate-limited requests ARE logged
//   - RateLimit early: Reject fast, before expensive auth
//   - Auth last: Only authenticated requests reach handler
//
// Request flow with rate limit block:
//
//	Recovery → Logging → RateLimit (BLOCK) → 429 → Logging(after) → Recovery(after)
//	                                               ↑ logs the 429!
//
// Why reverse loop?
//
//	We build the chain from inside out, so we iterate in reverse
//	to get the correct execution order.
func Chain(h http.Handler, middlewares ...Middleware) http.Handler {
	// Apply in reverse order so first middleware executes first
	for i := len(middlewares) - 1; i >= 0; i-- {
		h = middlewares[i](h)
	}
	return h
}

// ChainFunc is a convenience wrapper that accepts an http.HandlerFunc.
func ChainFunc(h http.HandlerFunc, middlewares ...Middleware) http.Handler {
	return Chain(h, middlewares...)
}
