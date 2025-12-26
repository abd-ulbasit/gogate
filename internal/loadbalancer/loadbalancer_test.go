package loadbalancer

import (
	"sync"
	"testing"

	"gogate/internal/backend"
)

// =============================================================================
// Test Scaffolding for Load Balancer Package
// =============================================================================
// These tests validate load balancing algorithms. Focus on:
// 1. Distribution fairness
// 2. Thread-safety under concurrent access
// 3. Handling of unhealthy backends
// 4. Empty pool edge cases
// =============================================================================

// TestRoundRobinDistribution verifies fair distribution
func TestRoundRobinDistribution(t *testing.T) {
	// Add backends
	b1 := backend.NewBackend("server1:8080")
	b2 := backend.NewBackend("server2:8080")
	b3 := backend.NewBackend("server3:8080")

	rr := NewRoundRobin([]*backend.Backend{b1, b2, b3})

	// Track distribution
	counts := make(map[string]int)

	// Call Next() 9 times (3 full rounds)
	for i := 0; i < 9; i++ {
		b, err := rr.Next()
		if err != nil {
			t.Fatalf("Next() error: %v", err)
		}
		if b == nil {
			t.Fatal("Next() returned nil")
		}
		counts[b.Addr()]++
	}

	// Each backend should have been selected 3 times
	for addr, count := range counts {
		if count != 3 {
			t.Errorf("backend %s selected %d times, want 3", addr, count)
		}
	}
}

// TestRoundRobinSkipsUnhealthy verifies unhealthy backends are skipped
func TestRoundRobinSkipsUnhealthy(t *testing.T) {
	b1 := backend.NewBackend("server1:8080")
	b2 := backend.NewBackend("server2:8080")
	b3 := backend.NewBackend("server3:8080")

	rr := NewRoundRobin([]*backend.Backend{b1, b2, b3})

	// Mark server2 as unhealthy
	b2.SetHealthy(false, nil)

	// Collect selections
	selected := make(map[string]bool)
	for i := 0; i < 10; i++ {
		b, _ := rr.Next()
		if b != nil {
			selected[b.Addr()] = true
		}
	}

	// server2 should never be selected
	if selected["server2:8080"] {
		t.Error("unhealthy backend was selected")
	}
}

// TestRoundRobinEmpty verifies behavior with no backends
func TestRoundRobinEmpty(t *testing.T) {
	rr := NewRoundRobin([]*backend.Backend{})

	b, err := rr.Next()
	if err != ErrNoBackends {
		t.Errorf("Next() error = %v, want ErrNoBackends", err)
	}
	if b != nil {
		t.Error("Next() should return nil when no backends")
	}
}

// TestRoundRobinAllUnhealthy verifies behavior when all backends are unhealthy
func TestRoundRobinAllUnhealthy(t *testing.T) {
	b1 := backend.NewBackend("server1:8080")
	b2 := backend.NewBackend("server2:8080")

	b1.SetHealthy(false, nil)
	b2.SetHealthy(false, nil)

	rr := NewRoundRobin([]*backend.Backend{b1, b2})

	b, err := rr.Next()
	if err != ErrNoHealthyBackends {
		t.Errorf("Next() error = %v, want ErrNoHealthyBackends", err)
	}
	if b != nil {
		t.Error("Next() should return nil when all backends unhealthy")
	}
}

// TestRoundRobinConcurrent tests thread-safety of round-robin selection
func TestRoundRobinConcurrent(t *testing.T) {
	backends := make([]*backend.Backend, 5)
	for i := 0; i < 5; i++ {
		backends[i] = backend.NewBackend("server" + string(rune('0'+i)) + ":8080")
	}
	rr := NewRoundRobin(backends)

	var wg sync.WaitGroup
	const numGoroutines = 100
	const selectionsPerGoroutine = 100

	wg.Add(numGoroutines)
	for i := 0; i < numGoroutines; i++ {
		go func() {
			defer wg.Done()
			for j := 0; j < selectionsPerGoroutine; j++ {
				b, err := rr.Next()
				if err != nil {
					t.Error("Next() returned error")
				}
				if b == nil {
					t.Error("Next() returned nil")
				}
			}
		}()
	}

	wg.Wait()
}

// TestLeastConnectionsPreference verifies least connections logic
// TODO(basit): LeastConnections requires internal connection tracking
// The Backend tracks connections through Dial() internally, so we can't
// easily simulate different connection counts without real connections.
// For now, we test the basic selection and empty pool behavior.
func TestLeastConnectionsPreference(t *testing.T) {
	b1 := backend.NewBackend("server1:8080")
	b2 := backend.NewBackend("server2:8080")
	b3 := backend.NewBackend("server3:8080")

	lc := NewLeastConnections([]*backend.Backend{b1, b2, b3})

	// With 0 connections each, should return first backend (tie-breaker)
	selected, err := lc.Next()
	if err != nil {
		t.Fatalf("Next() error: %v", err)
	}
	if selected == nil {
		t.Fatal("Next() returned nil")
	}
	if selected.Addr() != "server1:8080" {
		t.Errorf("Next() = %s, want server1:8080 (first on tie)", selected.Addr())
	}
}

// TestLeastConnectionsTieBreaker verifies tie-breaking behavior
func TestLeastConnectionsTieBreaker(t *testing.T) {
	b1 := backend.NewBackend("server1:8080")
	b2 := backend.NewBackend("server2:8080")

	lc := NewLeastConnections([]*backend.Backend{b1, b2})

	// Both have 0 connections - should return first one added
	selected, err := lc.Next()
	if err != nil {
		t.Fatalf("Next() error: %v", err)
	}
	if selected == nil {
		t.Fatal("Next() returned nil")
	}
	// First one added should be selected on tie
	if selected.Addr() != "server1:8080" {
		t.Errorf("tie-breaker: got %s, want server1:8080", selected.Addr())
	}
}

// TestLeastConnectionsSkipsUnhealthy verifies unhealthy backends are skipped
func TestLeastConnectionsSkipsUnhealthy(t *testing.T) {
	b1 := backend.NewBackend("server1:8080")
	b2 := backend.NewBackend("server2:8080")

	lc := NewLeastConnections([]*backend.Backend{b1, b2})

	// b1 is unhealthy
	b1.SetHealthy(false, nil)

	selected, err := lc.Next()
	if err != nil {
		t.Fatalf("Next() error: %v", err)
	}
	if selected == nil {
		t.Fatal("Next() returned nil")
	}
	if selected.Addr() != "server2:8080" {
		t.Error("should skip unhealthy backend")
	}
}

// TestRemoveBackend verifies backend removal
func TestRemoveBackend(t *testing.T) {
	b1 := backend.NewBackend("server1:8080")
	b2 := backend.NewBackend("server2:8080")

	rr := NewRoundRobin([]*backend.Backend{b1, b2})

	// Remove b1
	rr.RemoveBackend(b1.Addr())

	// Only b2 should be returned
	for i := 0; i < 5; i++ {
		selected, err := rr.Next()
		if err != nil {
			t.Fatalf("Next() error: %v", err)
		}
		if selected == nil {
			t.Fatal("Next() returned nil")
		}
		if selected.Addr() == "server1:8080" {
			t.Error("removed backend should not be selected")
		}
	}
}

// TestHealthyBackends verifies the Backends method returns all backends
func TestHealthyBackends(t *testing.T) {
	b1 := backend.NewBackend("server1:8080")
	b2 := backend.NewBackend("server2:8080")
	b3 := backend.NewBackend("server3:8080")

	rr := NewRoundRobin([]*backend.Backend{b1, b2, b3})

	// All backends returned
	backends := rr.Backends()
	if len(backends) != 3 {
		t.Errorf("Backends() = %d, want 3", len(backends))
	}

	// Mark one unhealthy - Backends() should still return all
	b2.SetHealthy(false, nil)

	backends = rr.Backends()
	if len(backends) != 3 {
		t.Errorf("Backends() after unhealthy = %d, want 3", len(backends))
	}
}

// =============================================================================
// Weighted Round-Robin Tests
// =============================================================================

// TestWeightedRoundRobinDistribution verifies weights affect distribution
func TestWeightedRoundRobinDistribution(t *testing.T) {
	// Backend with weight 5 should get 5x traffic of weight 1
	b1 := backend.NewBackendWithWeight("heavy:8080", 5)
	b2 := backend.NewBackendWithWeight("light1:8080", 1)
	b3 := backend.NewBackendWithWeight("light2:8080", 1)

	wrr := NewWeightedRoundRobin([]*backend.Backend{b1, b2, b3})

	counts := make(map[string]int)

	// Total weight = 7, run 70 iterations (10 full cycles)
	for i := 0; i < 70; i++ {
		b, err := wrr.Next()
		if err != nil {
			t.Fatalf("Next() error: %v", err)
		}
		counts[b.Addr()]++
	}

	// Expected: heavy=50, light1=10, light2=10
	if counts["heavy:8080"] != 50 {
		t.Errorf("heavy got %d requests, want 50", counts["heavy:8080"])
	}
	if counts["light1:8080"] != 10 {
		t.Errorf("light1 got %d requests, want 10", counts["light1:8080"])
	}
	if counts["light2:8080"] != 10 {
		t.Errorf("light2 got %d requests, want 10", counts["light2:8080"])
	}
}

// TestWeightedRoundRobinSmooth verifies smooth distribution (no bursts)
func TestWeightedRoundRobinSmooth(t *testing.T) {
	// Weights 2:1 should produce interleaved pattern, not AABAABAAB
	b1 := backend.NewBackendWithWeight("a:8080", 2)
	b2 := backend.NewBackendWithWeight("b:8080", 1)

	wrr := NewWeightedRoundRobin([]*backend.Backend{b1, b2})

	// Get 6 selections (2 full cycles of weight=3)
	sequence := make([]string, 6)
	for i := 0; i < 6; i++ {
		b, _ := wrr.Next()
		sequence[i] = b.Addr()
	}

	// With smooth WRR, should be: A, B, A, A, B, A (or similar smooth pattern)
	// The key is that 'b' is interleaved, not all at the end
	// Check that 'b' appears at positions 1 and 4 (or similar spread)
	bPositions := []int{}
	for i, addr := range sequence {
		if addr == "b:8080" {
			bPositions = append(bPositions, i)
		}
	}

	if len(bPositions) != 2 {
		t.Errorf("expected 2 selections of b, got %d", len(bPositions))
	}

	// Check that b selections are spread out (not consecutive)
	if len(bPositions) == 2 && bPositions[1]-bPositions[0] < 2 {
		t.Errorf("b selections should be spread out, got positions %v", bPositions)
	}
}

// TestWeightedRoundRobinSkipsUnhealthy verifies unhealthy backends are skipped
func TestWeightedRoundRobinSkipsUnhealthy(t *testing.T) {
	b1 := backend.NewBackendWithWeight("server1:8080", 3)
	b2 := backend.NewBackendWithWeight("server2:8080", 1)
	b3 := backend.NewBackendWithWeight("server3:8080", 1)

	wrr := NewWeightedRoundRobin([]*backend.Backend{b1, b2, b3})

	// Mark server1 (the heavy one) as unhealthy
	b1.SetHealthy(false, nil)

	// Now only server2 and server3 should be selected
	counts := make(map[string]int)
	for i := 0; i < 10; i++ {
		b, err := wrr.Next()
		if err != nil {
			t.Fatalf("Next() error: %v", err)
		}
		counts[b.Addr()]++
	}

	if counts["server1:8080"] != 0 {
		t.Error("unhealthy backend was selected")
	}
	// server2 and server3 have equal weight, should be 5 each
	if counts["server2:8080"] != 5 || counts["server3:8080"] != 5 {
		t.Errorf("expected equal distribution: server2=%d, server3=%d",
			counts["server2:8080"], counts["server3:8080"])
	}
}

// TestWeightedRoundRobinEmpty verifies behavior with no backends
func TestWeightedRoundRobinEmpty(t *testing.T) {
	wrr := NewWeightedRoundRobin([]*backend.Backend{})

	b, err := wrr.Next()
	if err != ErrNoBackends {
		t.Errorf("Next() error = %v, want ErrNoBackends", err)
	}
	if b != nil {
		t.Error("Next() should return nil when no backends")
	}
}

// TestWeightedRoundRobinAllUnhealthy verifies behavior when all backends unhealthy
func TestWeightedRoundRobinAllUnhealthy(t *testing.T) {
	b1 := backend.NewBackendWithWeight("server1:8080", 2)
	b2 := backend.NewBackendWithWeight("server2:8080", 1)

	b1.SetHealthy(false, nil)
	b2.SetHealthy(false, nil)

	wrr := NewWeightedRoundRobin([]*backend.Backend{b1, b2})

	b, err := wrr.Next()
	if err != ErrNoHealthyBackends {
		t.Errorf("Next() error = %v, want ErrNoHealthyBackends", err)
	}
	if b != nil {
		t.Error("Next() should return nil when all backends unhealthy")
	}
}

// TestWeightedRoundRobinConcurrent tests thread-safety
func TestWeightedRoundRobinConcurrent(t *testing.T) {
	backends := []*backend.Backend{
		backend.NewBackendWithWeight("server1:8080", 3),
		backend.NewBackendWithWeight("server2:8080", 2),
		backend.NewBackendWithWeight("server3:8080", 1),
	}
	wrr := NewWeightedRoundRobin(backends)

	var wg sync.WaitGroup
	const numGoroutines = 100
	const selectionsPerGoroutine = 100

	wg.Add(numGoroutines)
	for i := 0; i < numGoroutines; i++ {
		go func() {
			defer wg.Done()
			for j := 0; j < selectionsPerGoroutine; j++ {
				b, err := wrr.Next()
				if err != nil {
					t.Error("Next() returned error")
				}
				if b == nil {
					t.Error("Next() returned nil")
				}
			}
		}()
	}

	wg.Wait()
}

// TestWeightedRoundRobinAddRemove verifies dynamic backend management
func TestWeightedRoundRobinAddRemove(t *testing.T) {
	b1 := backend.NewBackendWithWeight("server1:8080", 1)
	wrr := NewWeightedRoundRobin([]*backend.Backend{b1})

	// Add a new backend
	b2 := backend.NewBackendWithWeight("server2:8080", 1)
	wrr.AddBackend(b2)

	backends := wrr.Backends()
	if len(backends) != 2 {
		t.Errorf("after AddBackend: got %d backends, want 2", len(backends))
	}

	// Remove original
	removed := wrr.RemoveBackend("server1:8080")
	if !removed {
		t.Error("RemoveBackend should return true for existing backend")
	}

	backends = wrr.Backends()
	if len(backends) != 1 {
		t.Errorf("after RemoveBackend: got %d backends, want 1", len(backends))
	}

	// Only server2 should be selected now
	b, _ := wrr.Next()
	if b.Addr() != "server2:8080" {
		t.Errorf("expected server2:8080, got %s", b.Addr())
	}
}
