package metrics

import (
	"bytes"
	"strings"
	"testing"
	"time"
)

func TestPrometheusExporter_Export(t *testing.T) {
	// Create collector with some data
	collector := NewCollector()
	collector.RecordConnection()
	collector.RecordConnection()
	collector.RecordConnectionClosed(100 * time.Millisecond)
	collector.RecordRequest("backend1:8080", true, 10*time.Millisecond, 100, 200)
	collector.RecordRequest("backend1:8080", false, 50*time.Millisecond, 50, 0)
	collector.RecordRequest("backend2:8080", true, 5*time.Millisecond, 80, 160)

	exporter := NewPrometheusExporter(collector)

	var buf bytes.Buffer
	err := exporter.Export(&buf)
	if err != nil {
		t.Fatalf("Export failed: %v", err)
	}

	output := buf.String()

	// Check for required metric types
	requiredMetrics := []string{
		"# HELP gogate_connections_total",
		"# TYPE gogate_connections_total counter",
		"gogate_connections_total 2",
		"# HELP gogate_connections_active",
		"gogate_connections_active 1",
		"# HELP gogate_requests_total",
		"gogate_requests_total{status=\"success\"} 2",
		"gogate_requests_total{status=\"failure\"} 1",
		"# HELP gogate_backend_requests_total",
		"gogate_backend_requests_total{backend=\"backend1:8080\",status=\"success\"} 1",
		"gogate_backend_requests_total{backend=\"backend1:8080\",status=\"failure\"} 1",
		"gogate_backend_requests_total{backend=\"backend2:8080\",status=\"success\"} 1",
		"# HELP gogate_uptime_seconds",
		"# TYPE gogate_uptime_seconds gauge",
	}

	for _, metric := range requiredMetrics {
		if !strings.Contains(output, metric) {
			t.Errorf("missing metric: %s\nOutput:\n%s", metric, output)
		}
	}
}

func TestPrometheusExporter_BackendHealth(t *testing.T) {
	collector := NewCollector()
	exporter := NewPrometheusExporter(collector)

	// Set up health getter
	exporter.SetBackendHealthGetter(func() map[string]bool {
		return map[string]bool{
			"backend1:8080": true,
			"backend2:8080": false,
		}
	})

	var buf bytes.Buffer
	err := exporter.Export(&buf)
	if err != nil {
		t.Fatalf("Export failed: %v", err)
	}

	output := buf.String()

	// Check health metrics
	if !strings.Contains(output, "gogate_backend_health{backend=\"backend1:8080\"} 1") {
		t.Errorf("missing healthy backend metric\nOutput:\n%s", output)
	}
	if !strings.Contains(output, "gogate_backend_health{backend=\"backend2:8080\"} 0") {
		t.Errorf("missing unhealthy backend metric\nOutput:\n%s", output)
	}
}

func TestPrometheusExporter_CircuitBreaker(t *testing.T) {
	collector := NewCollector()
	exporter := NewPrometheusExporter(collector)

	// Set up circuit breaker getter
	exporter.SetCircuitBreakerGetter(func() map[string]int {
		return map[string]int{
			"backend1:8080": 0, // closed
			"backend2:8080": 1, // open
			"backend3:8080": 2, // half-open
		}
	})

	var buf bytes.Buffer
	err := exporter.Export(&buf)
	if err != nil {
		t.Fatalf("Export failed: %v", err)
	}

	output := buf.String()

	// Check circuit breaker metrics
	if !strings.Contains(output, "gogate_circuit_breaker_state{backend=\"backend1:8080\"} 0") {
		t.Errorf("missing closed CB metric\nOutput:\n%s", output)
	}
	if !strings.Contains(output, "gogate_circuit_breaker_state{backend=\"backend2:8080\"} 1") {
		t.Errorf("missing open CB metric\nOutput:\n%s", output)
	}
	if !strings.Contains(output, "gogate_circuit_breaker_state{backend=\"backend3:8080\"} 2") {
		t.Errorf("missing half-open CB metric\nOutput:\n%s", output)
	}
}

func TestPrometheusExporter_RateLimiter(t *testing.T) {
	collector := NewCollector()
	exporter := NewPrometheusExporter(collector)

	// Set up rate limiter stats
	exporter.SetRateLimiterStats(func() (allowed, rejected int64) {
		return 1000, 50
	})

	var buf bytes.Buffer
	err := exporter.Export(&buf)
	if err != nil {
		t.Fatalf("Export failed: %v", err)
	}

	output := buf.String()

	if !strings.Contains(output, "gogate_rate_limiter_requests_total{result=\"allowed\"} 1000") {
		t.Errorf("missing allowed rate limiter metric\nOutput:\n%s", output)
	}
	if !strings.Contains(output, "gogate_rate_limiter_requests_total{result=\"rejected\"} 50") {
		t.Errorf("missing rejected rate limiter metric\nOutput:\n%s", output)
	}
}

func TestPrometheusExporter_PoolStats(t *testing.T) {
	collector := NewCollector()
	exporter := NewPrometheusExporter(collector)

	// Set up pool stats getter
	exporter.SetPoolStatsGetter(func() map[string]PoolStatsSnapshot {
		return map[string]PoolStatsSnapshot{
			"backend1:8080": {Hits: 100, Misses: 20, Size: 5},
		}
	})

	var buf bytes.Buffer
	err := exporter.Export(&buf)
	if err != nil {
		t.Fatalf("Export failed: %v", err)
	}

	output := buf.String()

	if !strings.Contains(output, "gogate_pool_hits_total{backend=\"backend1:8080\"} 100") {
		t.Errorf("missing pool hits metric\nOutput:\n%s", output)
	}
	if !strings.Contains(output, "gogate_pool_misses_total{backend=\"backend1:8080\"} 20") {
		t.Errorf("missing pool misses metric\nOutput:\n%s", output)
	}
	if !strings.Contains(output, "gogate_pool_size{backend=\"backend1:8080\"} 5") {
		t.Errorf("missing pool size metric\nOutput:\n%s", output)
	}
}

func TestPrometheusExporter_Histogram(t *testing.T) {
	collector := NewCollector()

	// Record various latencies to populate histogram
	collector.RecordLatency(500 * time.Microsecond) // < 1ms
	collector.RecordLatency(3 * time.Millisecond)   // <= 5ms
	collector.RecordLatency(8 * time.Millisecond)   // <= 10ms
	collector.RecordLatency(100 * time.Millisecond) // <= 100ms
	collector.RecordLatency(10 * time.Second)       // overflow

	exporter := NewPrometheusExporter(collector)

	var buf bytes.Buffer
	err := exporter.Export(&buf)
	if err != nil {
		t.Fatalf("Export failed: %v", err)
	}

	output := buf.String()

	// Check histogram format
	if !strings.Contains(output, "# TYPE gogate_request_duration_seconds histogram") {
		t.Errorf("missing histogram type declaration\nOutput:\n%s", output)
	}
	if !strings.Contains(output, "gogate_request_duration_seconds_bucket{le=") {
		t.Errorf("missing histogram buckets\nOutput:\n%s", output)
	}
	if !strings.Contains(output, "gogate_request_duration_seconds_count") {
		t.Errorf("missing histogram count\nOutput:\n%s", output)
	}
}

func TestFormatLabels(t *testing.T) {
	tests := []struct {
		name   string
		labels map[string]string
		want   string
	}{
		{
			name:   "empty labels",
			labels: nil,
			want:   "",
		},
		{
			name:   "single label",
			labels: map[string]string{"backend": "localhost:8080"},
			want:   `{backend="localhost:8080"}`,
		},
		{
			name:   "multiple labels sorted",
			labels: map[string]string{"status": "success", "backend": "localhost:8080"},
			want:   `{backend="localhost:8080",status="success"}`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := formatLabels(tt.labels)
			if got != tt.want {
				t.Errorf("formatLabels() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestEscapeLabelValue(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"simple", "simple"},
		{`with"quote`, `with\"quote`},
		{"with\\backslash", "with\\\\backslash"},
		{"with\nnewline", "with\\nnewline"},
	}

	for _, tt := range tests {
		got := escapeLabelValue(tt.input)
		if got != tt.want {
			t.Errorf("escapeLabelValue(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}

func TestRateLimiterWithStats(t *testing.T) {
	stats := NewRateLimiterWithStats()

	// Record some events
	for i := 0; i < 100; i++ {
		stats.RecordAllowed()
	}
	for i := 0; i < 10; i++ {
		stats.RecordRejected()
	}

	allowed, rejected := stats.Stats()
	if allowed != 100 {
		t.Errorf("allowed = %d, want 100", allowed)
	}
	if rejected != 10 {
		t.Errorf("rejected = %d, want 10", rejected)
	}
}

func TestRateLimiterWithStats_Concurrent(t *testing.T) {
	stats := NewRateLimiterWithStats()

	done := make(chan bool)
	for i := 0; i < 10; i++ {
		go func() {
			for j := 0; j < 100; j++ {
				stats.RecordAllowed()
				stats.RecordRejected()
			}
			done <- true
		}()
	}

	for i := 0; i < 10; i++ {
		<-done
	}

	allowed, rejected := stats.Stats()
	if allowed != 1000 {
		t.Errorf("allowed = %d, want 1000", allowed)
	}
	if rejected != 1000 {
		t.Errorf("rejected = %d, want 1000", rejected)
	}
}
