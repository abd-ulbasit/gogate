package metrics

import (
	"sync"
	"sync/atomic"
	"time"
)

// Metrics collects and exposes proxy statistics.
//
// Design:
// - In-memory counters (atomic for hot path)
// - Separate histograms for latency tracking
// - Thread-safe for concurrent updates
//
// Future: Export to Prometheus via /metrics endpoint
//
// Why in-memory first?
// 1. Simple to implement and test
// 2. No external dependencies
// 3. Easy to add Prometheus export later

// Collector holds all metrics for the proxy.
type Collector struct {
	// Connection metrics
	connectionsTotal    atomic.Int64 // Total connections received
	connectionsActive   atomic.Int64 // Currently active connections
	connectionsDuration atomic.Int64 // Total connection duration (nanoseconds)

	// Request metrics (if L7 proxy)
	requestsTotal    atomic.Int64
	requestsSuccess  atomic.Int64
	requestsFailures atomic.Int64

	// Backend metrics (per-backend, need map with mutex)
	mu             sync.RWMutex
	backendMetrics map[string]*BackendMetrics

	// Latency histogram (simplified buckets)
	latencyBuckets  []int64 // Counts for each bucket
	latencyBucketMs []int   // Bucket boundaries in ms

	startTime time.Time
}

// BackendMetrics holds per-backend statistics.
type BackendMetrics struct {
	RequestsTotal   atomic.Int64
	RequestsSuccess atomic.Int64
	RequestsFailed  atomic.Int64
	BytesSent       atomic.Int64
	BytesReceived   atomic.Int64
	LatencySum      atomic.Int64 // Total latency in nanoseconds
}

// NewCollector creates a new metrics collector.
func NewCollector() *Collector {
	// Default latency buckets: 1ms, 5ms, 10ms, 25ms, 50ms, 100ms, 250ms, 500ms, 1s, 5s
	buckets := []int{1, 5, 10, 25, 50, 100, 250, 500, 1000, 5000}

	return &Collector{
		backendMetrics:  make(map[string]*BackendMetrics),
		latencyBuckets:  make([]int64, len(buckets)+1), // +1 for overflow bucket
		latencyBucketMs: buckets,
		startTime:       time.Now(),
	}
}

// RecordConnection records a new connection.
func (c *Collector) RecordConnection() {
	c.connectionsTotal.Add(1)
	c.connectionsActive.Add(1)
}

// RecordConnectionClosed records a connection closing.
func (c *Collector) RecordConnectionClosed(duration time.Duration) {
	c.connectionsActive.Add(-1)
	c.connectionsDuration.Add(int64(duration))
}

// RecordRequest records a request to a backend.
func (c *Collector) RecordRequest(backend string, success bool, latency time.Duration, bytesSent, bytesReceived int64) {
	// Update global counters
	c.requestsTotal.Add(1)
	if success {
		c.requestsSuccess.Add(1)
	} else {
		c.requestsFailures.Add(1)
	}

	// Get or create backend metrics
	m := c.getOrCreateBackendMetrics(backend)
	m.RequestsTotal.Add(1)
	if success {
		m.RequestsSuccess.Add(1)
	} else {
		m.RequestsFailed.Add(1)
	}
	m.BytesSent.Add(bytesSent)
	m.BytesReceived.Add(bytesReceived)
	m.LatencySum.Add(int64(latency))

	// Record in latency histogram
	c.RecordLatency(latency)
}

// RecordLatency records a latency measurement in the histogram.
func (c *Collector) RecordLatency(d time.Duration) {
	ms := int(d.Milliseconds())
	bucket := len(c.latencyBucketMs) // Default to overflow bucket

	for i, boundary := range c.latencyBucketMs {
		if ms <= boundary {
			bucket = i
			break
		}
	}

	atomic.AddInt64(&c.latencyBuckets[bucket], 1)
}

// Snapshot returns a point-in-time snapshot of all metrics.
type Snapshot struct {
	Uptime            time.Duration
	ConnectionsTotal  int64
	ConnectionsActive int64
	AvgConnectionMs   float64

	RequestsTotal    int64
	RequestsSuccess  int64
	RequestsFailures int64

	Backends map[string]BackendSnapshot

	LatencyHistogram map[string]int64 // bucket label -> count
}

type BackendSnapshot struct {
	RequestsTotal   int64
	RequestsSuccess int64
	RequestsFailed  int64
	BytesSent       int64
	BytesReceived   int64
	AvgLatencyMs    float64
}

// Snapshot returns current metrics.
func (c *Collector) Snapshot() Snapshot {
	c.mu.RLock()
	backends := make(map[string]BackendSnapshot, len(c.backendMetrics))
	for addr, m := range c.backendMetrics {
		total := m.RequestsTotal.Load()
		avgLatency := float64(0)
		if total > 0 {
			avgLatency = float64(m.LatencySum.Load()) / float64(total) / float64(time.Millisecond)
		}
		backends[addr] = BackendSnapshot{
			RequestsTotal:   total,
			RequestsSuccess: m.RequestsSuccess.Load(),
			RequestsFailed:  m.RequestsFailed.Load(),
			BytesSent:       m.BytesSent.Load(),
			BytesReceived:   m.BytesReceived.Load(),
			AvgLatencyMs:    avgLatency,
		}
	}
	c.mu.RUnlock()

	connTotal := c.connectionsTotal.Load()
	avgConnMs := float64(0)
	if connTotal > 0 {
		avgConnMs = float64(c.connectionsDuration.Load()) / float64(connTotal) / float64(time.Millisecond)
	}

	// Build latency histogram labels
	histogram := make(map[string]int64)
	for i, boundary := range c.latencyBucketMs {
		label := "<=" + formatMs(boundary)
		histogram[label] = atomic.LoadInt64(&c.latencyBuckets[i])
	}
	histogram[">"+formatMs(c.latencyBucketMs[len(c.latencyBucketMs)-1])] = atomic.LoadInt64(&c.latencyBuckets[len(c.latencyBuckets)-1])

	return Snapshot{
		Uptime:            time.Since(c.startTime),
		ConnectionsTotal:  connTotal,
		ConnectionsActive: c.connectionsActive.Load(),
		AvgConnectionMs:   avgConnMs,
		RequestsTotal:     c.requestsTotal.Load(),
		RequestsSuccess:   c.requestsSuccess.Load(),
		RequestsFailures:  c.requestsFailures.Load(),
		Backends:          backends,
		LatencyHistogram:  histogram,
	}
}

func formatMs(ms int) string {
	if ms >= 1000 {
		return string(rune('0'+ms/1000)) + "s"
	}
	return string(rune('0'+ms/100)) + string(rune('0'+(ms/10)%10)) + string(rune('0'+ms%10)) + "ms"
}

// getOrCreateBackendMetrics gets or creates metrics for a backend.
func (c *Collector) getOrCreateBackendMetrics(addr string) *BackendMetrics {
	c.mu.RLock()
	m, ok := c.backendMetrics[addr]
	c.mu.RUnlock()
	if ok {
		return m
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	// Double-check after acquiring write lock
	if m, ok := c.backendMetrics[addr]; ok {
		return m
	}
	m = &BackendMetrics{}
	c.backendMetrics[addr] = m
	return m
}

// Reset resets all metrics. Useful for testing.
func (c *Collector) Reset() {
	c.connectionsTotal.Store(0)
	c.connectionsActive.Store(0)
	c.connectionsDuration.Store(0)
	c.requestsTotal.Store(0)
	c.requestsSuccess.Store(0)
	c.requestsFailures.Store(0)

	c.mu.Lock()
	c.backendMetrics = make(map[string]*BackendMetrics)
	for i := range c.latencyBuckets {
		c.latencyBuckets[i] = 0
	}
	c.mu.Unlock()
}
