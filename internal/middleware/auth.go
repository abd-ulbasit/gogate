package middleware

import (
	"context"
	"net/http"
	"strings"

	"gogate/internal/auth"
)

// contextKey is a custom type for context keys to avoid collisions.
//
// Why custom type? Standard practice to prevent key collisions.
// Two packages using string("user") as key would collide.
type contextKey string

const (
	// ClaimsContextKey is the context key for JWT claims.
	ClaimsContextKey contextKey = "jwt_claims"
)

// ClaimsFromContext retrieves JWT claims from the request context.
//
// Returns nil if no claims are present (e.g., unauthenticated request).
//
// Usage in handlers:
//
//	claims := middleware.ClaimsFromContext(r.Context())
//	if claims != nil {
//	    userID := claims.Subject
//	}
func ClaimsFromContext(ctx context.Context) *auth.Claims {
	claims, ok := ctx.Value(ClaimsContextKey).(*auth.Claims)
	if !ok {
		return nil
	}
	return claims
}

// AuthConfig configures the auth middleware behavior.
type AuthConfig struct {
	// Validator is the JWT validator instance.
	Validator *auth.Validator

	// Optional: Custom function to extract token from request.
	// Default: Extract from "Authorization: Bearer <token>" header.
	TokenExtractor func(r *http.Request) string

	// Optional: Custom error handler for auth failures.
	// Default: Returns 401 with error message.
	ErrorHandler func(w http.ResponseWriter, r *http.Request, err error)

	// Optional: Skip authentication for certain paths.
	// Returns true if the path should skip auth.
	// Example: Skip auth for /health, /metrics
	SkipAuth func(r *http.Request) bool
}

// Auth returns a middleware that validates JWT tokens.
//
// Authentication flow:
//  1. Check if path should skip auth (via SkipAuth config)
//  2. Extract token from Authorization header
//  3. Validate token (signature, expiration, claims)
//  4. Store claims in context for downstream handlers
//  5. If validation fails, return 401 Unauthorized
//
// Why middleware for auth?
//   - Single point of enforcement (DRY)
//   - Applied before business logic
//   - Claims available to all downstream handlers via context
//   - Route-level control via SkipAuth
//
// Example:
//
//	validator := auth.NewValidator(auth.ValidatorConfig{...})
//	authMiddleware := middleware.Auth(middleware.AuthConfig{
//	    Validator: validator,
//	    SkipAuth: func(r *http.Request) bool {
//	        return r.URL.Path == "/health"
//	    },
//	})
func Auth(config AuthConfig) Middleware {
	// Set defaults for optional config
	if config.TokenExtractor == nil {
		config.TokenExtractor = defaultTokenExtractor
	}
	if config.ErrorHandler == nil {
		config.ErrorHandler = defaultAuthErrorHandler
	}

	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// Check if this path should skip authentication
			if config.SkipAuth != nil && config.SkipAuth(r) {
				next.ServeHTTP(w, r)
				return
			}

			// Extract token from request
			tokenString := config.TokenExtractor(r)
			if tokenString == "" {
				config.ErrorHandler(w, r, auth.ErrInvalidToken)
				return
			}

			// Validate token
			token, err := config.Validator.Validate(tokenString)
			if err != nil {
				config.ErrorHandler(w, r, err)
				return
			}

			// Store claims in context for downstream handlers
			ctx := context.WithValue(r.Context(), ClaimsContextKey, &token.Claims)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// defaultTokenExtractor extracts Bearer token from Authorization header.
//
// Expected format: "Authorization: Bearer <token>"
//
// Why Bearer scheme?
//   - Standard OAuth 2.0 method (RFC 6750)
//   - Simple: No additional encoding needed
//   - Widely supported by clients and libraries
func defaultTokenExtractor(r *http.Request) string {
	authHeader := r.Header.Get("Authorization")
	if authHeader == "" {
		return ""
	}

	// Expected format: "Bearer <token>"
	parts := strings.SplitN(authHeader, " ", 2)
	if len(parts) != 2 || !strings.EqualFold(parts[0], "Bearer") {
		return ""
	}

	return parts[1]
}

// defaultAuthErrorHandler returns 401 Unauthorized with error message.
//
// Security note: We return a generic message to prevent information leakage.
// The actual error is not exposed (e.g., "invalid signature" vs "expired").
// Detailed errors would help attackers probe the auth system.
func defaultAuthErrorHandler(w http.ResponseWriter, r *http.Request, err error) {
	// Don't expose detailed error messages for security
	// Attacker shouldn't know if token is expired vs invalid signature
	w.Header().Set("WWW-Authenticate", `Bearer realm="api"`)
	http.Error(w, "Unauthorized", http.StatusUnauthorized)
}

// RequireAuth is a simpler auth middleware for common use cases.
//
// Use this when you don't need custom token extraction or error handling.
// For more control, use Auth() with AuthConfig.
func RequireAuth(validator *auth.Validator) Middleware {
	return Auth(AuthConfig{Validator: validator})
}

// OptionalAuth extracts JWT claims if present, but doesn't require them.
//
// Use for routes that should work with or without authentication.
// Example: A /profile endpoint that shows more data if authenticated.
//
// Difference from Auth():
//   - Auth(): Missing/invalid token → 401
//   - OptionalAuth(): Missing/invalid token → continue (no claims in context)
func OptionalAuth(validator *auth.Validator) Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			tokenString := defaultTokenExtractor(r)
			if tokenString != "" {
				// Try to validate, but don't fail if invalid
				if token, err := validator.Validate(tokenString); err == nil {
					ctx := context.WithValue(r.Context(), ClaimsContextKey, &token.Claims)
					r = r.WithContext(ctx)
				}
				// Invalid token: continue without claims (don't error)
			}
			next.ServeHTTP(w, r)
		})
	}
}
