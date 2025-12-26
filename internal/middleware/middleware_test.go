package middleware

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"gogate/internal/auth"
	"gogate/internal/ratelimiter"
)

func TestChain(t *testing.T) {
	// Track execution order
	order := []string{}

	m1 := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			order = append(order, "m1-before")
			next.ServeHTTP(w, r)
			order = append(order, "m1-after")
		})
	}

	m2 := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			order = append(order, "m2-before")
			next.ServeHTTP(w, r)
			order = append(order, "m2-after")
		})
	}

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		order = append(order, "handler")
	})

	chained := Chain(handler, m1, m2)

	req := httptest.NewRequest("GET", "/", nil)
	rec := httptest.NewRecorder()
	chained.ServeHTTP(rec, req)

	expected := []string{"m1-before", "m2-before", "handler", "m2-after", "m1-after"}
	if len(order) != len(expected) {
		t.Errorf("expected %d calls, got %d", len(expected), len(order))
	}
	for i, v := range expected {
		if order[i] != v {
			t.Errorf("position %d: expected %s, got %s", i, v, order[i])
		}
	}
}

func TestLoggingMiddleware(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, nil))

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("hello"))
	})

	logged := Logging(logger)(handler)

	req := httptest.NewRequest("GET", "/test?foo=bar", nil)
	req.Header.Set("User-Agent", "test-agent")
	rec := httptest.NewRecorder()

	logged.ServeHTTP(rec, req)

	// Check log output contains expected fields
	logOutput := buf.String()
	expectedFields := []string{
		`"method":"GET"`,
		`"path":"/test"`,
		`"query":"foo=bar"`,
		`"status":200`,
		`"bytes":5`,
		`"user_agent":"test-agent"`,
	}
	for _, field := range expectedFields {
		if !strings.Contains(logOutput, field) {
			t.Errorf("log missing field: %s\nlog: %s", field, logOutput)
		}
	}
}

func TestRecoveryMiddleware(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, nil))

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		panic("test panic")
	})

	recovered := Recovery(logger)(handler)

	req := httptest.NewRequest("GET", "/", nil)
	rec := httptest.NewRecorder()

	// Should not panic
	recovered.ServeHTTP(rec, req)

	// Should return 500
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("expected 500, got %d", rec.Code)
	}

	// Should log the panic
	logOutput := buf.String()
	if !strings.Contains(logOutput, "panic recovered") {
		t.Errorf("expected panic log, got: %s", logOutput)
	}
	if !strings.Contains(logOutput, "test panic") {
		t.Errorf("expected panic message in log, got: %s", logOutput)
	}
}

func TestRateLimitMiddleware(t *testing.T) {
	// Create limiter that allows 2 requests (burst), 0 rate (no refill)
	limiter := ratelimiter.NewTokenBucket(0, 2)

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	limited := RateLimit(limiter)(handler)

	// First two requests should succeed
	for i := 0; i < 2; i++ {
		req := httptest.NewRequest("GET", "/", nil)
		rec := httptest.NewRecorder()
		limited.ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			t.Errorf("request %d: expected 200, got %d", i+1, rec.Code)
		}
	}

	// Third request should be rate limited
	req := httptest.NewRequest("GET", "/", nil)
	rec := httptest.NewRecorder()
	limited.ServeHTTP(rec, req)

	if rec.Code != http.StatusTooManyRequests {
		t.Errorf("expected 429, got %d", rec.Code)
	}

	// Check Retry-After header
	if rec.Header().Get("Retry-After") == "" {
		t.Error("expected Retry-After header")
	}
}

func TestHeadersMiddleware(t *testing.T) {
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Check headers were set
		if r.Header.Get("X-Forwarded-For") == "" {
			t.Error("X-Forwarded-For not set")
		}
		if r.Header.Get("X-Real-IP") == "" {
			t.Error("X-Real-IP not set")
		}
		if r.Header.Get("X-Forwarded-Host") == "" {
			t.Error("X-Forwarded-Host not set")
		}
		if r.Header.Get("X-Forwarded-Proto") == "" {
			t.Error("X-Forwarded-Proto not set")
		}
		w.WriteHeader(http.StatusOK)
	})

	withHeaders := Headers()(handler)

	req := httptest.NewRequest("GET", "/", nil)
	req.Host = "example.com"
	req.RemoteAddr = "192.168.1.100:12345"
	rec := httptest.NewRecorder()

	withHeaders.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", rec.Code)
	}
}

func TestHeadersMiddlewareAppends(t *testing.T) {
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		xff := r.Header.Get("X-Forwarded-For")
		if !strings.Contains(xff, "10.0.0.1") {
			t.Errorf("expected original XFF preserved, got: %s", xff)
		}
		if !strings.Contains(xff, "192.168.1.100") {
			t.Errorf("expected new IP appended, got: %s", xff)
		}
		w.WriteHeader(http.StatusOK)
	})

	withHeaders := Headers()(handler)

	req := httptest.NewRequest("GET", "/", nil)
	req.Header.Set("X-Forwarded-For", "10.0.0.1")
	req.RemoteAddr = "192.168.1.100:12345"
	rec := httptest.NewRecorder()

	withHeaders.ServeHTTP(rec, req)
}

func TestRequestIDMiddleware(t *testing.T) {
	generated := "test-uuid-123"
	generator := func() string { return generated }

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Check request has ID
		if r.Header.Get("X-Request-ID") != generated {
			t.Errorf("expected request ID %s, got %s", generated, r.Header.Get("X-Request-ID"))
		}
		w.WriteHeader(http.StatusOK)
	})

	withRequestID := RequestID(generator)(handler)

	// Test new request gets ID
	req := httptest.NewRequest("GET", "/", nil)
	rec := httptest.NewRecorder()
	withRequestID.ServeHTTP(rec, req)

	// Check response has ID
	if rec.Header().Get("X-Request-ID") != generated {
		t.Errorf("expected response ID %s, got %s", generated, rec.Header().Get("X-Request-ID"))
	}

	// Test existing ID is preserved
	existingID := "existing-id"
	req = httptest.NewRequest("GET", "/", nil)
	req.Header.Set("X-Request-ID", existingID)
	rec = httptest.NewRecorder()

	preserveHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Request-ID") != existingID {
			t.Errorf("expected preserved ID %s, got %s", existingID, r.Header.Get("X-Request-ID"))
		}
	})

	RequestID(generator)(preserveHandler).ServeHTTP(rec, req)
}

func TestStripPrefixMiddleware(t *testing.T) {
	tests := []struct {
		name         string
		prefix       string
		requestPath  string
		expectedPath string
	}{
		{
			name:         "strips prefix",
			prefix:       "/api/v1",
			requestPath:  "/api/v1/users",
			expectedPath: "/users",
		},
		{
			name:         "no match leaves path unchanged",
			prefix:       "/api/v1",
			requestPath:  "/other/path",
			expectedPath: "/other/path",
		},
		{
			name:         "exact prefix becomes root",
			prefix:       "/api",
			requestPath:  "/api",
			expectedPath: "/",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != tt.expectedPath {
					t.Errorf("expected path %s, got %s", tt.expectedPath, r.URL.Path)
				}
			})

			stripped := StripPrefix(tt.prefix)(handler)

			req := httptest.NewRequest("GET", tt.requestPath, nil)
			rec := httptest.NewRecorder()
			stripped.ServeHTTP(rec, req)
		})
	}
}

func TestResponseWriterCapture(t *testing.T) {
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusCreated)
		w.Write([]byte("created"))
	})

	var captured *responseWriter
	wrapper := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			captured = wrapResponseWriter(w)
			next.ServeHTTP(captured, r)
		})
	}

	req := httptest.NewRequest("GET", "/", nil)
	rec := httptest.NewRecorder()
	wrapper(handler).ServeHTTP(rec, req)

	if captured.status != http.StatusCreated {
		t.Errorf("expected status 201, got %d", captured.status)
	}
	if captured.bytes != 7 {
		t.Errorf("expected 7 bytes, got %d", captured.bytes)
	}
}

// =============================================================================
// CORS Middleware Tests
// =============================================================================

func TestCORSPreflightRequest(t *testing.T) {
	config := CORSConfig{
		AllowedOrigins: []string{"https://example.com"},
		AllowedMethods: []string{"GET", "POST", "PUT"},
		AllowedHeaders: []string{"Authorization", "Content-Type"},
		MaxAge:         86400,
	}

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("handler should not be called for preflight")
	})

	corsMiddleware := CORS(config)(handler)

	// Preflight request (OPTIONS with Origin)
	req := httptest.NewRequest("OPTIONS", "/api/resource", nil)
	req.Header.Set("Origin", "https://example.com")
	req.Header.Set("Access-Control-Request-Method", "POST")
	rec := httptest.NewRecorder()

	corsMiddleware.ServeHTTP(rec, req)

	// Check status (should be 204 No Content)
	if rec.Code != http.StatusNoContent {
		t.Errorf("expected 204, got %d", rec.Code)
	}

	// Check CORS headers
	headers := []struct {
		name     string
		expected string
	}{
		{"Access-Control-Allow-Origin", "https://example.com"},
		{"Access-Control-Allow-Methods", "GET, POST, PUT"},
		{"Access-Control-Allow-Headers", "Authorization, Content-Type"},
		{"Access-Control-Max-Age", "86400"},
	}

	for _, h := range headers {
		if got := rec.Header().Get(h.name); got != h.expected {
			t.Errorf("%s = %q, want %q", h.name, got, h.expected)
		}
	}
}

func TestCORSActualRequest(t *testing.T) {
	config := CORSConfig{
		AllowedOrigins:   []string{"https://example.com"},
		AllowedMethods:   []string{"GET", "POST"},
		AllowCredentials: true,
		ExposedHeaders:   []string{"X-Request-ID"},
	}

	handlerCalled := false
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		handlerCalled = true
		w.WriteHeader(http.StatusOK)
	})

	corsMiddleware := CORS(config)(handler)

	// Actual request (GET with Origin)
	req := httptest.NewRequest("GET", "/api/resource", nil)
	req.Header.Set("Origin", "https://example.com")
	rec := httptest.NewRecorder()

	corsMiddleware.ServeHTTP(rec, req)

	// Handler should be called
	if !handlerCalled {
		t.Error("handler should be called for actual request")
	}

	// Check CORS headers
	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "https://example.com" {
		t.Errorf("Access-Control-Allow-Origin = %q, want https://example.com", got)
	}
	if got := rec.Header().Get("Access-Control-Allow-Credentials"); got != "true" {
		t.Errorf("Access-Control-Allow-Credentials = %q, want true", got)
	}
	if got := rec.Header().Get("Access-Control-Expose-Headers"); got != "X-Request-ID" {
		t.Errorf("Access-Control-Expose-Headers = %q, want X-Request-ID", got)
	}
}

func TestCORSDisallowedOrigin(t *testing.T) {
	config := CORSConfig{
		AllowedOrigins: []string{"https://allowed.com"},
	}

	handlerCalled := false
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		handlerCalled = true
		w.WriteHeader(http.StatusOK)
	})

	corsMiddleware := CORS(config)(handler)

	// Request from disallowed origin
	req := httptest.NewRequest("GET", "/api/resource", nil)
	req.Header.Set("Origin", "https://evil.com")
	rec := httptest.NewRecorder()

	corsMiddleware.ServeHTTP(rec, req)

	// Handler should still be called (CORS doesn't block server-side)
	if !handlerCalled {
		t.Error("handler should be called even for disallowed origin")
	}

	// But no CORS headers should be set (browser will block)
	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "" {
		t.Errorf("Access-Control-Allow-Origin should be empty, got %q", got)
	}
}

func TestCORSWildcardOrigin(t *testing.T) {
	config := DefaultCORSConfig() // Uses "*" for AllowedOrigins

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	corsMiddleware := CORS(config)(handler)

	req := httptest.NewRequest("GET", "/api/resource", nil)
	req.Header.Set("Origin", "https://any-origin.com")
	rec := httptest.NewRecorder()

	corsMiddleware.ServeHTTP(rec, req)

	// Should allow any origin with "*"
	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "*" {
		t.Errorf("Access-Control-Allow-Origin = %q, want *", got)
	}
}

func TestCORSSameOriginNoHeaders(t *testing.T) {
	config := CORSConfig{
		AllowedOrigins: []string{"https://example.com"},
	}

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	corsMiddleware := CORS(config)(handler)

	// Request without Origin header (same-origin)
	req := httptest.NewRequest("GET", "/api/resource", nil)
	rec := httptest.NewRecorder()

	corsMiddleware.ServeHTTP(rec, req)

	// No CORS headers for same-origin
	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "" {
		t.Errorf("Access-Control-Allow-Origin should be empty for same-origin, got %q", got)
	}
}

// =============================================================================
// Auth Middleware Tests
// =============================================================================

func TestAuthMiddlewareValidToken(t *testing.T) {
	// Create a test token using the same method as jwt_test.go
	tokenString := createTestToken(t, "user123", time.Now().Add(time.Hour).Unix())

	validator := createTestValidator()

	handlerCalled := false
	var capturedClaims *auth.Claims

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		handlerCalled = true
		capturedClaims = ClaimsFromContext(r.Context())
		w.WriteHeader(http.StatusOK)
	})

	authMiddleware := Auth(AuthConfig{Validator: validator})(handler)

	req := httptest.NewRequest("GET", "/api/resource", nil)
	req.Header.Set("Authorization", "Bearer "+tokenString)
	rec := httptest.NewRecorder()

	authMiddleware.ServeHTTP(rec, req)

	if !handlerCalled {
		t.Error("handler should be called for valid token")
	}
	if rec.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", rec.Code)
	}
	if capturedClaims == nil {
		t.Error("claims should be in context")
	}
	if capturedClaims != nil && capturedClaims.Subject != "user123" {
		t.Errorf("Subject = %q, want user123", capturedClaims.Subject)
	}
}

func TestAuthMiddlewareMissingToken(t *testing.T) {
	validator := createTestValidator()

	handlerCalled := false
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		handlerCalled = true
	})

	authMiddleware := Auth(AuthConfig{Validator: validator})(handler)

	req := httptest.NewRequest("GET", "/api/resource", nil)
	// No Authorization header
	rec := httptest.NewRecorder()

	authMiddleware.ServeHTTP(rec, req)

	if handlerCalled {
		t.Error("handler should not be called for missing token")
	}
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("expected 401, got %d", rec.Code)
	}
}

func TestAuthMiddlewareInvalidToken(t *testing.T) {
	validator := createTestValidator()

	handlerCalled := false
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		handlerCalled = true
	})

	authMiddleware := Auth(AuthConfig{Validator: validator})(handler)

	req := httptest.NewRequest("GET", "/api/resource", nil)
	req.Header.Set("Authorization", "Bearer invalid.token.here")
	rec := httptest.NewRecorder()

	authMiddleware.ServeHTTP(rec, req)

	if handlerCalled {
		t.Error("handler should not be called for invalid token")
	}
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("expected 401, got %d", rec.Code)
	}
}

func TestAuthMiddlewareExpiredToken(t *testing.T) {
	// Create expired token
	tokenString := createTestToken(t, "user123", time.Now().Add(-time.Hour).Unix())

	validator := createTestValidator()

	handlerCalled := false
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		handlerCalled = true
	})

	authMiddleware := Auth(AuthConfig{Validator: validator})(handler)

	req := httptest.NewRequest("GET", "/api/resource", nil)
	req.Header.Set("Authorization", "Bearer "+tokenString)
	rec := httptest.NewRecorder()

	authMiddleware.ServeHTTP(rec, req)

	if handlerCalled {
		t.Error("handler should not be called for expired token")
	}
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("expected 401, got %d", rec.Code)
	}
}

func TestAuthMiddlewareSkipAuth(t *testing.T) {
	validator := createTestValidator()

	handlerCalled := false
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		handlerCalled = true
		w.WriteHeader(http.StatusOK)
	})

	authMiddleware := Auth(AuthConfig{
		Validator: validator,
		SkipAuth: func(r *http.Request) bool {
			return r.URL.Path == "/health"
		},
	})(handler)

	// Request to skipped path (no token required)
	req := httptest.NewRequest("GET", "/health", nil)
	rec := httptest.NewRecorder()

	authMiddleware.ServeHTTP(rec, req)

	if !handlerCalled {
		t.Error("handler should be called for skipped path")
	}
	if rec.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", rec.Code)
	}
}

func TestOptionalAuthWithToken(t *testing.T) {
	tokenString := createTestToken(t, "user123", time.Now().Add(time.Hour).Unix())
	validator := createTestValidator()

	var capturedClaims *auth.Claims
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedClaims = ClaimsFromContext(r.Context())
		w.WriteHeader(http.StatusOK)
	})

	optionalAuth := OptionalAuth(validator)(handler)

	req := httptest.NewRequest("GET", "/api/resource", nil)
	req.Header.Set("Authorization", "Bearer "+tokenString)
	rec := httptest.NewRecorder()

	optionalAuth.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", rec.Code)
	}
	if capturedClaims == nil {
		t.Error("claims should be in context when token provided")
	}
}

func TestOptionalAuthWithoutToken(t *testing.T) {
	validator := createTestValidator()

	var capturedClaims *auth.Claims
	handlerCalled := false
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		handlerCalled = true
		capturedClaims = ClaimsFromContext(r.Context())
		w.WriteHeader(http.StatusOK)
	})

	optionalAuth := OptionalAuth(validator)(handler)

	req := httptest.NewRequest("GET", "/api/resource", nil)
	// No Authorization header
	rec := httptest.NewRecorder()

	optionalAuth.ServeHTTP(rec, req)

	if !handlerCalled {
		t.Error("handler should be called even without token")
	}
	if rec.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", rec.Code)
	}
	if capturedClaims != nil {
		t.Error("claims should be nil when no token provided")
	}
}

func TestOptionalAuthWithInvalidToken(t *testing.T) {
	validator := createTestValidator()

	var capturedClaims *auth.Claims
	handlerCalled := false
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		handlerCalled = true
		capturedClaims = ClaimsFromContext(r.Context())
		w.WriteHeader(http.StatusOK)
	})

	optionalAuth := OptionalAuth(validator)(handler)

	req := httptest.NewRequest("GET", "/api/resource", nil)
	req.Header.Set("Authorization", "Bearer invalid.token.here")
	rec := httptest.NewRecorder()

	optionalAuth.ServeHTTP(rec, req)

	// Should still call handler (optional auth doesn't fail on invalid)
	if !handlerCalled {
		t.Error("handler should be called even with invalid token")
	}
	if rec.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", rec.Code)
	}
	// Claims should be nil (invalid token)
	if capturedClaims != nil {
		t.Error("claims should be nil when token is invalid")
	}
}

// =============================================================================
// Test Helpers
// =============================================================================

var testAuthSecretKey = []byte("test-secret-key-for-middleware-tests")

// createTestValidator creates a JWT validator for testing.
func createTestValidator() *auth.Validator {
	return auth.NewValidator(auth.ValidatorConfig{
		SecretKey: testAuthSecretKey,
	})
}

// createTestToken creates a signed HS256 JWT for testing.
func createTestToken(t *testing.T, subject string, exp int64) string {
	t.Helper()

	header := `{"alg":"HS256","typ":"JWT"}`
	payload := fmt.Sprintf(`{"sub":"%s","exp":%d}`, subject, exp)

	encodedHeader := base64URLEncode([]byte(header))
	encodedPayload := base64URLEncode([]byte(payload))
	signedPortion := encodedHeader + "." + encodedPayload

	h := hmac.New(sha256.New, testAuthSecretKey)
	h.Write([]byte(signedPortion))
	signature := base64URLEncode(h.Sum(nil))

	return signedPortion + "." + signature
}

// base64URLEncode encodes bytes to base64url.
func base64URLEncode(data []byte) string {
	return strings.TrimRight(base64.URLEncoding.EncodeToString(data), "=")
}
