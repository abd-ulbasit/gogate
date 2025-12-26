package observability

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestNewLogger_JSON(t *testing.T) {
	var buf bytes.Buffer
	cfg := LogConfig{
		Level:  "info",
		Format: "json",
		Output: &buf,
	}

	logger := NewLogger(cfg)
	logger.Info("test message", "key", "value")

	output := buf.String()
	if !strings.Contains(output, "test message") {
		t.Errorf("expected log to contain message, got: %s", output)
	}
	if !strings.Contains(output, `"key":"value"`) {
		t.Errorf("expected log to contain key-value, got: %s", output)
	}
}

func TestNewLogger_Text(t *testing.T) {
	var buf bytes.Buffer
	cfg := LogConfig{
		Level:  "debug",
		Format: "text",
		Output: &buf,
	}

	logger := NewLogger(cfg)
	logger.Debug("debug message")

	output := buf.String()
	if !strings.Contains(output, "debug message") {
		t.Errorf("expected log to contain message, got: %s", output)
	}
}

func TestNewLogger_LevelFiltering(t *testing.T) {
	var buf bytes.Buffer
	cfg := LogConfig{
		Level:  "warn",
		Format: "json",
		Output: &buf,
	}

	logger := NewLogger(cfg)
	logger.Info("info message")  // Should be filtered
	logger.Warn("warn message")  // Should appear

	output := buf.String()
	if strings.Contains(output, "info message") {
		t.Errorf("info should be filtered at warn level, got: %s", output)
	}
	if !strings.Contains(output, "warn message") {
		t.Errorf("warn should appear, got: %s", output)
	}
}

func TestRequestID(t *testing.T) {
	id1 := RequestID()
	id2 := RequestID()

	// Should be 16 hex characters
	if len(id1) != 16 {
		t.Errorf("expected 16 char ID, got %d: %s", len(id1), id1)
	}

	// Should be unique
	if id1 == id2 {
		t.Error("request IDs should be unique")
	}

	// Should be valid hex
	for _, c := range id1 {
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')) {
			t.Errorf("invalid hex character: %c", c)
		}
	}
}

func TestRequestIDContext(t *testing.T) {
	ctx := context.Background()

	// No ID initially
	if id := RequestIDFromContext(ctx); id != "" {
		t.Errorf("expected empty ID, got: %s", id)
	}

	// Add ID
	ctx = WithRequestID(ctx, "test-request-id")
	if id := RequestIDFromContext(ctx); id != "test-request-id" {
		t.Errorf("expected 'test-request-id', got: %s", id)
	}
}

func TestTrace(t *testing.T) {
	trace := NewTrace("test.operation")

	// Check ID was generated
	if len(trace.ID) != 16 {
		t.Errorf("expected 16 char ID, got %d", len(trace.ID))
	}

	// Check name
	if trace.Name != "test.operation" {
		t.Errorf("expected 'test.operation', got: %s", trace.Name)
	}

	// Add events
	trace.AddEvent("started")
	time.Sleep(10 * time.Millisecond)
	trace.AddEvent("processing")
	trace.End()

	// Check events
	if len(trace.Events) != 2 {
		t.Errorf("expected 2 events, got %d", len(trace.Events))
	}
	if trace.Events[0].Name != "started" {
		t.Errorf("expected 'started', got: %s", trace.Events[0].Name)
	}

	// Check duration
	if trace.Duration() < 10*time.Millisecond {
		t.Errorf("expected duration >= 10ms, got: %v", trace.Duration())
	}
}

func TestTraceAttributes(t *testing.T) {
	trace := NewTrace("test")
	trace.SetAttr("backend", "localhost:8080")
	trace.SetAttr("route", "api-route")

	if trace.Attrs["backend"] != "localhost:8080" {
		t.Errorf("expected 'localhost:8080', got: %s", trace.Attrs["backend"])
	}
	if trace.Attrs["route"] != "api-route" {
		t.Errorf("expected 'api-route', got: %s", trace.Attrs["route"])
	}
}

func TestTraceContext(t *testing.T) {
	ctx := context.Background()

	// No trace initially
	if trace := TraceFromContext(ctx); trace != nil {
		t.Error("expected nil trace")
	}

	// Add trace
	trace := NewTrace("test")
	ctx = WithTrace(ctx, trace)

	// Should get trace back
	got := TraceFromContext(ctx)
	if got != trace {
		t.Error("expected same trace from context")
	}

	// Request ID should also be set
	if RequestIDFromContext(ctx) != trace.ID {
		t.Error("expected request ID from trace in context")
	}
}

func TestLoggerWithTrace(t *testing.T) {
	var buf bytes.Buffer
	logger := NewLogger(LogConfig{
		Level:  "info",
		Format: "json",
		Output: &buf,
	})

	trace := NewTrace("http.request")
	trace.SetAttr("backend", "localhost:8080")

	enriched := LoggerWithTrace(logger, trace)
	enriched.Info("test message")

	// Parse JSON to check fields
	var entry map[string]interface{}
	if err := json.Unmarshal(buf.Bytes(), &entry); err != nil {
		t.Fatalf("failed to parse JSON: %v", err)
	}

	if entry["request_id"] != trace.ID {
		t.Errorf("expected request_id=%s, got: %v", trace.ID, entry["request_id"])
	}
	if entry["trace"] != "http.request" {
		t.Errorf("expected trace=http.request, got: %v", entry["trace"])
	}
	if entry["backend"] != "localhost:8080" {
		t.Errorf("expected backend=localhost:8080, got: %v", entry["backend"])
	}
}

func TestLoggerWithTrace_NilTrace(t *testing.T) {
	var buf bytes.Buffer
	logger := NewLogger(LogConfig{
		Level:  "info",
		Format: "json",
		Output: &buf,
	})

	// Should not panic with nil trace
	enriched := LoggerWithTrace(logger, nil)
	enriched.Info("test message")

	if buf.Len() == 0 {
		t.Error("expected log output")
	}
}

func TestLoggerContext(t *testing.T) {
	ctx := context.Background()

	// Default logger if not in context
	logger := LoggerFromContext(ctx)
	if logger == nil {
		t.Error("expected default logger")
	}

	// Custom logger
	var buf bytes.Buffer
	custom := NewLogger(LogConfig{Output: &buf})
	ctx = WithLogger(ctx, custom)

	got := LoggerFromContext(ctx)
	got.Info("test")

	if buf.Len() == 0 {
		t.Error("expected log from custom logger")
	}
}

func TestTimer(t *testing.T) {
	timer := NewTimer()
	time.Sleep(10 * time.Millisecond)

	elapsed := timer.Elapsed()
	if elapsed < 10*time.Millisecond {
		t.Errorf("expected >= 10ms, got: %v", elapsed)
	}

	ms := timer.ElapsedMs()
	if ms < 10 {
		t.Errorf("expected >= 10ms, got: %.2fms", ms)
	}

	str := timer.String()
	if !strings.Contains(str, "ms") {
		t.Errorf("expected ms in string, got: %s", str)
	}
}

func TestTrace_Concurrent(t *testing.T) {
	trace := NewTrace("concurrent.test")

	done := make(chan bool)
	for i := 0; i < 10; i++ {
		go func(n int) {
			for j := 0; j < 100; j++ {
				trace.AddEvent("event")
				trace.SetAttr("key", "value")
			}
			done <- true
		}(i)
	}

	for i := 0; i < 10; i++ {
		<-done
	}

	// Just checking it doesn't panic/race
	if len(trace.Events) != 1000 {
		t.Errorf("expected 1000 events, got %d", len(trace.Events))
	}
}
