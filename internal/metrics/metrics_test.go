package metrics

import (
	"testing"
	"time"
)

func TestCollectorConnectionMetrics(t *testing.T) {
	c := NewCollector()

	// Record some connections
	c.RecordConnection()
	c.RecordConnection()
	c.RecordConnection()

	snap := c.Snapshot()
	if snap.ConnectionsTotal != 3 {
		t.Errorf("expected 3 total connections, got %d", snap.ConnectionsTotal)
	}
	if snap.ConnectionsActive != 3 {
		t.Errorf("expected 3 active connections, got %d", snap.ConnectionsActive)
	}

	// Close one
	c.RecordConnectionClosed(100 * time.Millisecond)

	snap = c.Snapshot()
	if snap.ConnectionsActive != 2 {
		t.Errorf("expected 2 active connections after close, got %d", snap.ConnectionsActive)
	}
}

func TestCollectorLatencyHistogram(t *testing.T) {
	c := NewCollector()

	// Record various latencies
	c.RecordLatency(500 * time.Microsecond) // < 1ms bucket
	c.RecordLatency(3 * time.Millisecond)   // <= 5ms bucket
	c.RecordLatency(8 * time.Millisecond)   // <= 10ms bucket
	c.RecordLatency(100 * time.Millisecond) // <= 100ms bucket
	c.RecordLatency(10 * time.Second)       // overflow bucket

	snap := c.Snapshot()

	// Check histogram has entries
	if len(snap.LatencyHistogram) == 0 {
		t.Error("expected non-empty latency histogram")
	}

	// Just verify we recorded something
	total := int64(0)
	for _, count := range snap.LatencyHistogram {
		total += count
	}
	if total != 5 {
		t.Errorf("expected 5 total latency records, got %d", total)
	}
}

func TestCollectorBackendMetrics(t *testing.T) {
	c := NewCollector()

	// Record requests to different backends
	c.RecordRequest("backend1:8080", true, 10*time.Millisecond, 100, 200)
	c.RecordRequest("backend1:8080", true, 20*time.Millisecond, 150, 250)
	c.RecordRequest("backend1:8080", false, 30*time.Millisecond, 50, 0)
	c.RecordRequest("backend2:8080", true, 5*time.Millisecond, 80, 160)

	snap := c.Snapshot()

	// Check backend1
	b1, ok := snap.Backends["backend1:8080"]
	if !ok {
		t.Fatal("expected backend1 in metrics")
	}
	if b1.RequestsTotal != 3 {
		t.Errorf("backend1: expected 3 total, got %d", b1.RequestsTotal)
	}
	if b1.RequestsSuccess != 2 {
		t.Errorf("backend1: expected 2 success, got %d", b1.RequestsSuccess)
	}
	if b1.RequestsFailed != 1 {
		t.Errorf("backend1: expected 1 failed, got %d", b1.RequestsFailed)
	}

	// Check backend2
	b2, ok := snap.Backends["backend2:8080"]
	if !ok {
		t.Fatal("expected backend2 in metrics")
	}
	if b2.RequestsTotal != 1 {
		t.Errorf("backend2: expected 1 total, got %d", b2.RequestsTotal)
	}
}

func TestCollectorReset(t *testing.T) {
	c := NewCollector()

	c.RecordConnection()
	c.RecordRequest("backend:8080", true, 10*time.Millisecond, 100, 200)

	c.Reset()

	snap := c.Snapshot()
	if snap.ConnectionsTotal != 0 {
		t.Errorf("expected 0 connections after reset, got %d", snap.ConnectionsTotal)
	}
	if len(snap.Backends) != 0 {
		t.Errorf("expected 0 backends after reset, got %d", len(snap.Backends))
	}
}

func TestCollectorUptime(t *testing.T) {
	c := NewCollector()

	time.Sleep(50 * time.Millisecond)

	snap := c.Snapshot()
	if snap.Uptime < 50*time.Millisecond {
		t.Errorf("expected uptime >= 50ms, got %v", snap.Uptime)
	}
}

func TestCollectorConcurrent(t *testing.T) {
	c := NewCollector()

	done := make(chan bool)
	for i := 0; i < 10; i++ {
		go func() {
			for j := 0; j < 100; j++ {
				c.RecordConnection()
				c.RecordLatency(time.Duration(j) * time.Millisecond)
				c.RecordConnectionClosed(time.Duration(j) * time.Millisecond)
			}
			done <- true
		}()
	}

	for i := 0; i < 10; i++ {
		<-done
	}

	// Just checking it doesn't panic/deadlock
	snap := c.Snapshot()
	if snap.ConnectionsTotal != 1000 {
		t.Errorf("expected 1000 total connections, got %d", snap.ConnectionsTotal)
	}
}
