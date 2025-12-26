package metrics

import (
	"fmt"
	"io"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// PrometheusExporter exports metrics in Prometheus text format.
//
// Prometheus text format specification:
// https://prometheus.io/docs/instrumenting/exposition_formats/
//
// Format:
//   # HELP metric_name Description
//   # TYPE metric_name type
//   metric_name{label="value"} value timestamp
//
// Design decisions:
// - Pure Go implementation (no prometheus client dependency)
// - Thread-safe for concurrent scrapes
// - Efficient string building with strings.Builder
// - Namespace prefix "gogate_" for all metrics
//
// Why build our own exporter?
// 1. Learning - understand Prometheus format internals
// 2. No dependency - keeps gogate dependency-free
// 3. Control - expose exactly what we need
type PrometheusExporter struct {
	collector *Collector
	namespace string

	// Additional metrics sources
	mu                  sync.RWMutex
	backendHealthGetter func() map[string]bool            // backend addr -> healthy
	circuitBreakerGetter func() map[string]int            // backend addr -> state (0=closed, 1=open, 2=half-open)
	rateLimiterStats    func() (allowed, rejected int64)  // rate limiter stats
	poolStatsGetter     func() map[string]PoolStatsSnapshot // backend addr -> pool stats
}

// PoolStatsSnapshot holds pool statistics for Prometheus export.
type PoolStatsSnapshot struct {
	Hits     int64
	Misses   int64
	Returned int64
	Dropped  int64
	Size     int
}

// NewPrometheusExporter creates a new Prometheus exporter.
func NewPrometheusExporter(collector *Collector) *PrometheusExporter {
	return &PrometheusExporter{
		collector: collector,
		namespace: "gogate",
	}
}

// SetBackendHealthGetter sets the function to get backend health status.
func (e *PrometheusExporter) SetBackendHealthGetter(fn func() map[string]bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.backendHealthGetter = fn
}

// SetCircuitBreakerGetter sets the function to get circuit breaker states.
func (e *PrometheusExporter) SetCircuitBreakerGetter(fn func() map[string]int) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.circuitBreakerGetter = fn
}

// SetRateLimiterStats sets the function to get rate limiter statistics.
func (e *PrometheusExporter) SetRateLimiterStats(fn func() (allowed, rejected int64)) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.rateLimiterStats = fn
}

// SetPoolStatsGetter sets the function to get connection pool statistics.
func (e *PrometheusExporter) SetPoolStatsGetter(fn func() map[string]PoolStatsSnapshot) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.poolStatsGetter = fn
}

// Export writes all metrics in Prometheus text format to the writer.
func (e *PrometheusExporter) Export(w io.Writer) error {
	var b strings.Builder

	// Get snapshot from collector
	snap := e.collector.Snapshot()

	// === Connection Metrics ===
	e.writeHelp(&b, "connections_total", "Total TCP connections received")
	e.writeType(&b, "connections_total", "counter")
	e.writeMetric(&b, "connections_total", nil, snap.ConnectionsTotal)

	e.writeHelp(&b, "connections_active", "Currently active TCP connections")
	e.writeType(&b, "connections_active", "gauge")
	e.writeMetric(&b, "connections_active", nil, snap.ConnectionsActive)

	e.writeHelp(&b, "connection_duration_seconds_total", "Total time spent in connections")
	e.writeType(&b, "connection_duration_seconds_total", "counter")
	// Convert from our internal representation (avg in ms) to total seconds
	totalDurationSec := float64(snap.ConnectionsTotal) * snap.AvgConnectionMs / 1000.0
	e.writeMetricFloat(&b, "connection_duration_seconds_total", nil, totalDurationSec)

	// === Request Metrics ===
	e.writeHelp(&b, "requests_total", "Total requests processed")
	e.writeType(&b, "requests_total", "counter")
	e.writeMetric(&b, "requests_total", map[string]string{"status": "success"}, snap.RequestsSuccess)
	e.writeMetric(&b, "requests_total", map[string]string{"status": "failure"}, snap.RequestsFailures)

	// === Backend Metrics ===
	e.writeHelp(&b, "backend_requests_total", "Total requests per backend")
	e.writeType(&b, "backend_requests_total", "counter")
	for addr, bs := range snap.Backends {
		e.writeMetric(&b, "backend_requests_total", map[string]string{"backend": addr, "status": "success"}, bs.RequestsSuccess)
		e.writeMetric(&b, "backend_requests_total", map[string]string{"backend": addr, "status": "failure"}, bs.RequestsFailed)
	}

	e.writeHelp(&b, "backend_bytes_sent_total", "Total bytes sent to backends")
	e.writeType(&b, "backend_bytes_sent_total", "counter")
	for addr, bs := range snap.Backends {
		e.writeMetric(&b, "backend_bytes_sent_total", map[string]string{"backend": addr}, bs.BytesSent)
	}

	e.writeHelp(&b, "backend_bytes_received_total", "Total bytes received from backends")
	e.writeType(&b, "backend_bytes_received_total", "counter")
	for addr, bs := range snap.Backends {
		e.writeMetric(&b, "backend_bytes_received_total", map[string]string{"backend": addr}, bs.BytesReceived)
	}

	e.writeHelp(&b, "backend_latency_seconds", "Average backend latency in seconds")
	e.writeType(&b, "backend_latency_seconds", "gauge")
	for addr, bs := range snap.Backends {
		e.writeMetricFloat(&b, "backend_latency_seconds", map[string]string{"backend": addr}, bs.AvgLatencyMs/1000.0)
	}

	// === Latency Histogram ===
	e.writeHelp(&b, "request_duration_seconds", "Request duration histogram")
	e.writeType(&b, "request_duration_seconds", "histogram")
	e.writeHistogram(&b, "request_duration_seconds", snap.LatencyHistogram)

	// === Uptime ===
	e.writeHelp(&b, "uptime_seconds", "Time since proxy started")
	e.writeType(&b, "uptime_seconds", "gauge")
	e.writeMetricFloat(&b, "uptime_seconds", nil, snap.Uptime.Seconds())

	// === Backend Health (from external getter) ===
	e.mu.RLock()
	healthGetter := e.backendHealthGetter
	cbGetter := e.circuitBreakerGetter
	rlStats := e.rateLimiterStats
	poolGetter := e.poolStatsGetter
	e.mu.RUnlock()

	if healthGetter != nil {
		healthMap := healthGetter()
		e.writeHelp(&b, "backend_health", "Backend health status (1=healthy, 0=unhealthy)")
		e.writeType(&b, "backend_health", "gauge")
		for addr, healthy := range healthMap {
			val := int64(0)
			if healthy {
				val = 1
			}
			e.writeMetric(&b, "backend_health", map[string]string{"backend": addr}, val)
		}
	}

	// === Circuit Breaker State ===
	if cbGetter != nil {
		cbMap := cbGetter()
		e.writeHelp(&b, "circuit_breaker_state", "Circuit breaker state (0=closed, 1=open, 2=half_open)")
		e.writeType(&b, "circuit_breaker_state", "gauge")
		for addr, state := range cbMap {
			e.writeMetric(&b, "circuit_breaker_state", map[string]string{"backend": addr}, int64(state))
		}
	}

	// === Rate Limiter ===
	if rlStats != nil {
		allowed, rejected := rlStats()
		e.writeHelp(&b, "rate_limiter_requests_total", "Rate limiter request counts")
		e.writeType(&b, "rate_limiter_requests_total", "counter")
		e.writeMetric(&b, "rate_limiter_requests_total", map[string]string{"result": "allowed"}, allowed)
		e.writeMetric(&b, "rate_limiter_requests_total", map[string]string{"result": "rejected"}, rejected)
	}

	// === Connection Pool Stats ===
	if poolGetter != nil {
		poolMap := poolGetter()
		if len(poolMap) > 0 {
			e.writeHelp(&b, "pool_hits_total", "Connection pool cache hits")
			e.writeType(&b, "pool_hits_total", "counter")
			for addr, ps := range poolMap {
				e.writeMetric(&b, "pool_hits_total", map[string]string{"backend": addr}, ps.Hits)
			}

			e.writeHelp(&b, "pool_misses_total", "Connection pool cache misses")
			e.writeType(&b, "pool_misses_total", "counter")
			for addr, ps := range poolMap {
				e.writeMetric(&b, "pool_misses_total", map[string]string{"backend": addr}, ps.Misses)
			}

			e.writeHelp(&b, "pool_size", "Current connection pool size")
			e.writeType(&b, "pool_size", "gauge")
			for addr, ps := range poolMap {
				e.writeMetric(&b, "pool_size", map[string]string{"backend": addr}, int64(ps.Size))
			}
		}
	}

	// Write to output
	_, err := io.WriteString(w, b.String())
	return err
}

// writeHelp writes a HELP line.
func (e *PrometheusExporter) writeHelp(b *strings.Builder, name, help string) {
	fmt.Fprintf(b, "# HELP %s_%s %s\n", e.namespace, name, help)
}

// writeType writes a TYPE line.
func (e *PrometheusExporter) writeType(b *strings.Builder, name, metricType string) {
	fmt.Fprintf(b, "# TYPE %s_%s %s\n", e.namespace, name, metricType)
}

// writeMetric writes a metric line with integer value.
func (e *PrometheusExporter) writeMetric(b *strings.Builder, name string, labels map[string]string, value int64) {
	fmt.Fprintf(b, "%s_%s%s %d\n", e.namespace, name, formatLabels(labels), value)
}

// writeMetricFloat writes a metric line with float value.
func (e *PrometheusExporter) writeMetricFloat(b *strings.Builder, name string, labels map[string]string, value float64) {
	fmt.Fprintf(b, "%s_%s%s %g\n", e.namespace, name, formatLabels(labels), value)
}

// writeHistogram writes histogram buckets in Prometheus format.
//
// Input format from collector: {"<=1ms": 10, "<=5ms": 20, ...}
// Output format: metric_bucket{le="0.001"} 10
func (e *PrometheusExporter) writeHistogram(b *strings.Builder, name string, histogram map[string]int64) {
	// Convert our ms-based buckets to seconds for Prometheus
	// Our format: "<=1ms", "<=5ms", ..., ">5000ms"
	// Prometheus format: le="0.001", le="0.005", ...

	type bucket struct {
		le    float64 // less-than-or-equal boundary in seconds
		count int64
		label string // for +Inf
	}

	var buckets []bucket
	var totalCount int64

	for label, count := range histogram {
		totalCount += count
		if strings.HasPrefix(label, "<=") {
			// Parse "<=100ms" -> 0.1
			msStr := strings.TrimSuffix(strings.TrimPrefix(label, "<="), "ms")
			msStr = strings.TrimSuffix(msStr, "s") // Handle "<=1s" format
			var ms float64
			fmt.Sscanf(msStr, "%f", &ms)
			// Check if it was seconds (e.g., "1s") or milliseconds
			if strings.HasSuffix(strings.TrimPrefix(label, "<="), "s") && !strings.HasSuffix(strings.TrimPrefix(label, "<="), "ms") {
				buckets = append(buckets, bucket{le: ms, count: count})
			} else {
				buckets = append(buckets, bucket{le: ms / 1000.0, count: count})
			}
		} else if strings.HasPrefix(label, ">") {
			// Overflow bucket - will be added as +Inf
			buckets = append(buckets, bucket{le: -1, count: count, label: "+Inf"})
		}
	}

	// Sort buckets by le value (with +Inf last)
	sort.Slice(buckets, func(i, j int) bool {
		if buckets[i].le < 0 {
			return false // +Inf goes last
		}
		if buckets[j].le < 0 {
			return true
		}
		return buckets[i].le < buckets[j].le
	})

	// Write cumulative buckets (Prometheus histograms are cumulative)
	var cumulative int64
	for _, bkt := range buckets {
		cumulative += bkt.count
		if bkt.le < 0 {
			// +Inf bucket
			fmt.Fprintf(b, "%s_%s_bucket{le=\"+Inf\"} %d\n", e.namespace, name, cumulative)
		} else {
			fmt.Fprintf(b, "%s_%s_bucket{le=\"%g\"} %d\n", e.namespace, name, bkt.le, cumulative)
		}
	}

	// Write _count and _sum
	fmt.Fprintf(b, "%s_%s_count %d\n", e.namespace, name, totalCount)
	// Note: We don't have exact sum, so we skip _sum or estimate it
}

// formatLabels formats labels as {key="value",key2="value2"}.
func formatLabels(labels map[string]string) string {
	if len(labels) == 0 {
		return ""
	}

	// Sort keys for deterministic output
	keys := make([]string, 0, len(labels))
	for k := range labels {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	var b strings.Builder
	b.WriteString("{")
	for i, k := range keys {
		if i > 0 {
			b.WriteString(",")
		}
		// Escape label values
		fmt.Fprintf(&b, "%s=\"%s\"", k, escapeLabelValue(labels[k]))
	}
	b.WriteString("}")
	return b.String()
}

// escapeLabelValue escapes special characters in label values.
func escapeLabelValue(s string) string {
	// Prometheus requires escaping: \ " \n
	s = strings.ReplaceAll(s, "\\", "\\\\")
	s = strings.ReplaceAll(s, "\"", "\\\"")
	s = strings.ReplaceAll(s, "\n", "\\n")
	return s
}

// RateLimiterWithStats wraps a rate limiter to track allowed/rejected counts.
//
// This is a decorator pattern - wraps existing rate limiter to add metrics.
type RateLimiterWithStats struct {
	allowed  atomic.Int64
	rejected atomic.Int64
}

// NewRateLimiterWithStats creates a new rate limiter stats tracker.
func NewRateLimiterWithStats() *RateLimiterWithStats {
	return &RateLimiterWithStats{}
}

// RecordAllowed increments the allowed counter.
func (r *RateLimiterWithStats) RecordAllowed() {
	r.allowed.Add(1)
}

// RecordRejected increments the rejected counter.
func (r *RateLimiterWithStats) RecordRejected() {
	r.rejected.Add(1)
}

// Stats returns current allowed and rejected counts.
func (r *RateLimiterWithStats) Stats() (allowed, rejected int64) {
	return r.allowed.Load(), r.rejected.Load()
}

// Handler returns an HTTP handler that exports Prometheus metrics.
func (e *PrometheusExporter) Handler() func(w io.Writer) error {
	return e.Export
}

// ProcessInfo holds process information for metrics.
type ProcessInfo struct {
	StartTime time.Time
}

// NewProcessInfo creates process info with current start time.
func NewProcessInfo() *ProcessInfo {
	return &ProcessInfo{
		StartTime: time.Now(),
	}
}
