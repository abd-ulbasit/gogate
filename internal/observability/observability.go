// Package observability provides structured logging, request tracing, and metrics
// infrastructure for the gogate proxy.
//
// Key components:
// - Logger: Configured slog.Logger with JSON output and context enrichment
// - RequestID: Correlation ID generation and propagation
// - Tracer: Request lifecycle tracing with timing
//
// Design principles:
// 1. Context-based propagation - request context carries trace info
// 2. Zero allocation on hot path where possible
// 3. Compatible with OpenTelemetry concepts (span-like tracing)
// 4. Pure Go - no external dependencies
package observability

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"log/slog"
	"os"
	"sync"
	"time"
)

// contextKey is a custom type to avoid context key collisions.
type contextKey int

const (
	requestIDKey contextKey = iota
	traceKey
	loggerKey
)

// LogConfig configures the structured logger.
type LogConfig struct {
	// Level is the minimum log level (debug, info, warn, error)
	Level string

	// Format is the output format (json, text)
	Format string

	// Output is where logs are written (defaults to stdout)
	Output io.Writer

	// AddSource adds source file/line to log entries (expensive, for debugging)
	AddSource bool
}

// DefaultLogConfig returns sensible defaults for production.
func DefaultLogConfig() LogConfig {
	return LogConfig{
		Level:     "info",
		Format:    "json",
		Output:    os.Stdout,
		AddSource: false,
	}
}

// NewLogger creates a configured slog.Logger.
//
// Design:
// - JSON format for structured log aggregation (ELK, Loki, etc.)
// - Text format for local development
// - Level filtering to reduce noise in production
func NewLogger(cfg LogConfig) *slog.Logger {
	if cfg.Output == nil {
		cfg.Output = os.Stdout
	}

	// Parse level
	var level slog.Level
	switch cfg.Level {
	case "debug":
		level = slog.LevelDebug
	case "warn":
		level = slog.LevelWarn
	case "error":
		level = slog.LevelError
	default:
		level = slog.LevelInfo
	}

	opts := &slog.HandlerOptions{
		Level:     level,
		AddSource: cfg.AddSource,
	}

	var handler slog.Handler
	switch cfg.Format {
	case "text":
		handler = slog.NewTextHandler(cfg.Output, opts)
	default:
		handler = slog.NewJSONHandler(cfg.Output, opts)
	}

	return slog.New(handler)
}

// RequestID generates a unique request ID.
//
// Format: 16 random hex characters (64 bits of entropy)
// Example: "a1b2c3d4e5f6a7b8"
//
// Why not UUID?
// - Shorter (16 vs 36 chars) - less log/header overhead
// - 64 bits is enough for request correlation
// - Simpler to generate (just random bytes)
func RequestID() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b) // crypto/rand never fails in practice
	return hex.EncodeToString(b)
}

// WithRequestID adds a request ID to the context.
func WithRequestID(ctx context.Context, requestID string) context.Context {
	return context.WithValue(ctx, requestIDKey, requestID)
}

// RequestIDFromContext extracts the request ID from context.
// Returns empty string if not present.
func RequestIDFromContext(ctx context.Context) string {
	if v := ctx.Value(requestIDKey); v != nil {
		return v.(string)
	}
	return ""
}

// Trace represents a request trace with timing information.
//
// Similar to OpenTelemetry Span but simplified:
// - Start/end times
// - Named events with timestamps
// - Attributes (key-value pairs)
//
// Not a full distributed tracing implementation, but provides
// request lifecycle visibility for debugging and metrics.
type Trace struct {
	ID        string            // Request ID (correlation)
	Name      string            // Operation name (e.g., "http.request", "tcp.proxy")
	StartTime time.Time         // When trace started
	EndTime   time.Time         // When trace ended (zero if ongoing)
	Events    []TraceEvent      // Named events within the trace
	Attrs     map[string]string // Attributes (backend, route, etc.)

	mu sync.Mutex // Protects Events and Attrs
}

// TraceEvent represents a named event within a trace.
type TraceEvent struct {
	Name string
	Time time.Time
}

// NewTrace creates a new trace with the given name.
func NewTrace(name string) *Trace {
	return &Trace{
		ID:        RequestID(),
		Name:      name,
		StartTime: time.Now(),
		Attrs:     make(map[string]string),
	}
}

// AddEvent adds a named event to the trace.
func (t *Trace) AddEvent(name string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.Events = append(t.Events, TraceEvent{
		Name: name,
		Time: time.Now(),
	})
}

// SetAttr sets an attribute on the trace.
func (t *Trace) SetAttr(key, value string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.Attrs[key] = value
}

// End marks the trace as complete.
func (t *Trace) End() {
	t.EndTime = time.Now()
}

// Duration returns the trace duration.
// If trace is ongoing, returns time since start.
func (t *Trace) Duration() time.Duration {
	if t.EndTime.IsZero() {
		return time.Since(t.StartTime)
	}
	return t.EndTime.Sub(t.StartTime)
}

// WithTrace adds a trace to the context.
func WithTrace(ctx context.Context, trace *Trace) context.Context {
	// Also set request ID from trace
	ctx = WithRequestID(ctx, trace.ID)
	return context.WithValue(ctx, traceKey, trace)
}

// TraceFromContext extracts the trace from context.
// Returns nil if not present.
func TraceFromContext(ctx context.Context) *Trace {
	if v := ctx.Value(traceKey); v != nil {
		return v.(*Trace)
	}
	return nil
}

// LoggerWithTrace returns a logger enriched with trace context.
//
// Adds to every log entry:
// - request_id: Correlation ID
// - trace_name: Operation name
// - Any trace attributes
func LoggerWithTrace(logger *slog.Logger, trace *Trace) *slog.Logger {
	if trace == nil {
		return logger
	}

	attrs := []any{
		"request_id", trace.ID,
		"trace", trace.Name,
	}

	trace.mu.Lock()
	for k, v := range trace.Attrs {
		attrs = append(attrs, k, v)
	}
	trace.mu.Unlock()

	return logger.With(attrs...)
}

// WithLogger adds a logger to the context.
func WithLogger(ctx context.Context, logger *slog.Logger) context.Context {
	return context.WithValue(ctx, loggerKey, logger)
}

// LoggerFromContext extracts the logger from context.
// Returns the default logger if not present.
func LoggerFromContext(ctx context.Context) *slog.Logger {
	if v := ctx.Value(loggerKey); v != nil {
		return v.(*slog.Logger)
	}
	return slog.Default()
}

// LogRequest logs an HTTP request with trace context.
func LogRequest(logger *slog.Logger, trace *Trace, method, path string, status int, duration time.Duration) {
	l := LoggerWithTrace(logger, trace)
	l.Info("http.request",
		"method", method,
		"path", path,
		"status", status,
		"duration_ms", float64(duration.Microseconds())/1000,
	)
}

// LogTCPConnection logs a TCP connection event.
func LogTCPConnection(logger *slog.Logger, trace *Trace, event string, backend string, bytesSent, bytesRecv int64, duration time.Duration) {
	l := LoggerWithTrace(logger, trace)
	l.Info("tcp.connection",
		"event", event,
		"backend", backend,
		"bytes_sent", bytesSent,
		"bytes_recv", bytesRecv,
		"duration_ms", float64(duration.Microseconds())/1000,
	)
}

// LogError logs an error with trace context.
func LogError(logger *slog.Logger, trace *Trace, msg string, err error) {
	l := LoggerWithTrace(logger, trace)
	l.Error(msg, "error", err)
}

// Timer is a simple timer for measuring operation duration.
//
// Usage:
//
//	timer := observability.NewTimer()
//	// ... do work ...
//	duration := timer.Elapsed()
type Timer struct {
	start time.Time
}

// NewTimer creates a new timer starting now.
func NewTimer() *Timer {
	return &Timer{start: time.Now()}
}

// Elapsed returns the time since the timer started.
func (t *Timer) Elapsed() time.Duration {
	return time.Since(t.start)
}

// ElapsedMs returns elapsed time in milliseconds as a float.
func (t *Timer) ElapsedMs() float64 {
	return float64(t.Elapsed().Microseconds()) / 1000
}

// String implements fmt.Stringer for Timer.
func (t *Timer) String() string {
	return fmt.Sprintf("%.3fms", t.ElapsedMs())
}
