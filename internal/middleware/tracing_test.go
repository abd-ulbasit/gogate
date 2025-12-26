package middleware

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"gogate/internal/metrics"
	"gogate/internal/observability"
	"gogate/internal/ratelimiter"
)

func TestTracing(t *testing.T) {
	var buf strings.Builder
	logger := observability.NewLogger(observability.LogConfig{
		Level:  "info",
		Format: "json",
		Output: &buf,
	})

	handler := Tracing(logger)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Check that request ID is set
		requestID := r.Header.Get("X-Request-ID")
		if requestID == "" {
			t.Error("expected X-Request-ID header to be set")
		}

		// Check that trace is in context
		trace := observability.TraceFromContext(r.Context())
		if trace == nil {
			t.Error("expected trace in context")
			return
		}
		if trace.ID != requestID {
			t.Errorf("trace ID mismatch: %s != %s", trace.ID, requestID)
		}

		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest("GET", "/test", nil)
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	// Check response has X-Request-ID
	if rec.Header().Get("X-Request-ID") == "" {
		t.Error("expected X-Request-ID in response")
	}

	// Check that log was written
	if buf.Len() == 0 {
		t.Error("expected log output")
	}
	if !strings.Contains(buf.String(), "request.complete") {
		t.Errorf("expected 'request.complete' in log, got: %s", buf.String())
	}
}

func TestTracing_PreservesExistingRequestID(t *testing.T) {
	logger := observability.NewLogger(observability.LogConfig{
		Level:  "info",
		Format: "json",
	})

	handler := Tracing(logger)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestID := r.Header.Get("X-Request-ID")
		if requestID != "existing-request-id" {
			t.Errorf("expected preserved request ID, got: %s", requestID)
		}

		trace := observability.TraceFromContext(r.Context())
		if trace.ID != "existing-request-id" {
			t.Errorf("expected trace ID to match existing, got: %s", trace.ID)
		}

		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest("GET", "/test", nil)
	req.Header.Set("X-Request-ID", "existing-request-id")
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Header().Get("X-Request-ID") != "existing-request-id" {
		t.Error("expected response to have preserved request ID")
	}
}

func TestTracingWithRequestID(t *testing.T) {
	handler := TracingWithRequestID()(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestID := r.Header.Get("X-Request-ID")
		if requestID == "" {
			t.Error("expected X-Request-ID header")
		}
		if len(requestID) != 16 {
			t.Errorf("expected 16 char request ID, got %d", len(requestID))
		}

		// Check context
		ctxID := observability.RequestIDFromContext(r.Context())
		if ctxID != requestID {
			t.Errorf("context ID mismatch: %s != %s", ctxID, requestID)
		}

		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest("GET", "/test", nil)
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Header().Get("X-Request-ID") == "" {
		t.Error("expected X-Request-ID in response")
	}
}

func TestMetricsMiddleware(t *testing.T) {
	collector := metrics.NewCollector()

	handler := Metrics(collector)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("hello"))
	}))

	req := httptest.NewRequest("GET", "/api/test", nil)
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	// Check metrics were recorded
	snap := collector.Snapshot()
	if snap.RequestsTotal != 1 {
		t.Errorf("expected 1 request, got %d", snap.RequestsTotal)
	}
	if snap.RequestsSuccess != 1 {
		t.Errorf("expected 1 success, got %d", snap.RequestsSuccess)
	}
}

func TestMetricsMiddleware_ErrorStatus(t *testing.T) {
	collector := metrics.NewCollector()

	handler := Metrics(collector)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))

	req := httptest.NewRequest("GET", "/api/test", nil)
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	snap := collector.Snapshot()
	if snap.RequestsFailures != 1 {
		t.Errorf("expected 1 failure, got %d", snap.RequestsFailures)
	}
}

func TestRateLimitWithMetrics(t *testing.T) {
	rl := ratelimiter.NewTokenBucket(10, 5) // 10 req/sec, burst 5
	stats := metrics.NewRateLimiterWithStats()

	handler := RateLimitWithMetrics(rl, stats)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	// Make some requests (within rate limit)
	for i := 0; i < 5; i++ {
		req := httptest.NewRequest("GET", "/test", nil)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			t.Errorf("expected 200, got %d", rec.Code)
		}
	}

	allowed, rejected := stats.Stats()
	if allowed != 5 {
		t.Errorf("expected 5 allowed, got %d", allowed)
	}
	if rejected != 0 {
		t.Errorf("expected 0 rejected, got %d", rejected)
	}
}

func TestRateLimitWithMetrics_Exceeded(t *testing.T) {
	rl := ratelimiter.NewTokenBucket(0.1, 1) // Very low rate
	stats := metrics.NewRateLimiterWithStats()

	handler := RateLimitWithMetrics(rl, stats)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	// First request should succeed
	req := httptest.NewRequest("GET", "/test", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	// Second request should be rejected
	req = httptest.NewRequest("GET", "/test", nil)
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusTooManyRequests {
		t.Errorf("expected 429, got %d", rec.Code)
	}

	allowed, rejected := stats.Stats()
	if allowed != 1 {
		t.Errorf("expected 1 allowed, got %d", allowed)
	}
	if rejected != 1 {
		t.Errorf("expected 1 rejected, got %d", rejected)
	}
}

func TestLoggerWithTraceMiddleware(t *testing.T) {
	var buf strings.Builder
	logger := observability.NewLogger(observability.LogConfig{
		Level:  "info",
		Format: "json",
		Output: &buf,
	})

	// Chain: Tracing -> LoggingWithTrace -> handler
	baseHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	handler := Chain(baseHandler,
		Tracing(logger),
		LoggingWithTrace(logger),
	)

	req := httptest.NewRequest("GET", "/test", nil)
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	output := buf.String()
	// Should have request_id in log entries
	if !strings.Contains(output, "request_id") {
		t.Errorf("expected request_id in log, got: %s", output)
	}
}
