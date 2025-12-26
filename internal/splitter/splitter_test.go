package splitter

import (
	"math"
	"sync"
	"testing"

	"gogate/internal/backend"
	"gogate/internal/loadbalancer"
)

// mockLoadBalancer implements loadbalancer.LoadBalancer for testing.
type mockLoadBalancer struct {
	name string
}

func (m *mockLoadBalancer) Next() (*backend.Backend, error) {
	return backend.NewBackend(m.name + ":8080"), nil
}
func (m *mockLoadBalancer) Backends() []*backend.Backend      { return nil }
func (m *mockLoadBalancer) AddBackend(*backend.Backend)       {}
func (m *mockLoadBalancer) RemoveBackend(addr string) bool    { return false }
func (m *mockLoadBalancer) UpdateBackends([]*backend.Backend) {}

func TestSplitterSinglePool100Percent(t *testing.T) {
	// 100% to pool A, 0% to anything else
	// All requests should go to pool A
	poolA := &mockLoadBalancer{name: "pool-a"}

	splitter := NewSimpleSplitter()
	splitter.AddPool(Pool{
		Name:       "production",
		Weight:     100,
		Balancer:   poolA,
		Predicates: nil,
	})

	// All requests should go to production
	for i := 0; i < 100; i++ {
		lb, poolName := splitter.Route()
		mockLB, ok := lb.(*mockLoadBalancer)
		if !ok || mockLB.name != "pool-a" {
			t.Errorf("expected pool-a, got different pool")
		}
		if poolName != "production" {
			t.Errorf("expected pool name 'production', got %s", poolName)
		}
	}
}

func TestSplitterTwoPools5050(t *testing.T) {
	// 50/50 split between two pools
	poolA := &mockLoadBalancer{name: "pool-a"}
	poolB := &mockLoadBalancer{name: "pool-b"}

	splitter := NewSimpleSplitter()
	splitter.AddPool(Pool{Name: "stable", Weight: 50, Balancer: poolA})
	splitter.AddPool(Pool{Name: "canary", Weight: 50, Balancer: poolB})

	counts := make(map[string]int)
	iterations := 1000

	for i := 0; i < iterations; i++ {
		_, poolName := splitter.Route()
		counts[poolName]++
	}

	// Should be roughly 50/50 (allow 10% tolerance)
	stableRatio := float64(counts["stable"]) / float64(iterations)
	canaryRatio := float64(counts["canary"]) / float64(iterations)

	if math.Abs(stableRatio-0.5) > 0.1 {
		t.Errorf("stable ratio = %.2f, want ~0.5", stableRatio)
	}
	if math.Abs(canaryRatio-0.5) > 0.1 {
		t.Errorf("canary ratio = %.2f, want ~0.5", canaryRatio)
	}
}

func TestSplitterCanaryDeployment(t *testing.T) {
	// 95/5 split (canary deployment pattern)
	poolProd := &mockLoadBalancer{name: "production"}
	poolCanary := &mockLoadBalancer{name: "canary"}

	splitter := NewSimpleSplitter()
	splitter.AddPool(Pool{Name: "production", Weight: 95, Balancer: poolProd})
	splitter.AddPool(Pool{Name: "canary", Weight: 5, Balancer: poolCanary})

	counts := make(map[string]int)
	iterations := 10000

	for i := 0; i < iterations; i++ {
		_, poolName := splitter.Route()
		counts[poolName]++
	}

	// Production should get ~95%, canary ~5%
	prodRatio := float64(counts["production"]) / float64(iterations)
	canaryRatio := float64(counts["canary"]) / float64(iterations)

	// Allow 2% tolerance for statistical variance
	if math.Abs(prodRatio-0.95) > 0.02 {
		t.Errorf("production ratio = %.3f, want ~0.95", prodRatio)
	}
	if math.Abs(canaryRatio-0.05) > 0.02 {
		t.Errorf("canary ratio = %.3f, want ~0.05", canaryRatio)
	}
}

func TestSplitterThreePools(t *testing.T) {
	// 60/30/10 split across three pools
	poolA := &mockLoadBalancer{name: "pool-a"}
	poolB := &mockLoadBalancer{name: "pool-b"}
	poolC := &mockLoadBalancer{name: "pool-c"}

	splitter := NewSimpleSplitter()
	splitter.AddPool(Pool{Name: "primary", Weight: 60, Balancer: poolA})
	splitter.AddPool(Pool{Name: "secondary", Weight: 30, Balancer: poolB})
	splitter.AddPool(Pool{Name: "tertiary", Weight: 10, Balancer: poolC})

	counts := make(map[string]int)
	iterations := 10000

	for i := 0; i < iterations; i++ {
		_, poolName := splitter.Route()
		counts[poolName]++
	}

	// Check ratios with 3% tolerance
	tests := []struct {
		name     string
		expected float64
	}{
		{"primary", 0.60},
		{"secondary", 0.30},
		{"tertiary", 0.10},
	}

	for _, tt := range tests {
		ratio := float64(counts[tt.name]) / float64(iterations)
		if math.Abs(ratio-tt.expected) > 0.03 {
			t.Errorf("%s ratio = %.3f, want ~%.2f", tt.name, ratio, tt.expected)
		}
	}
}

func TestSplitterConcurrent(t *testing.T) {
	// Test thread safety with concurrent routing
	poolA := &mockLoadBalancer{name: "pool-a"}
	poolB := &mockLoadBalancer{name: "pool-b"}

	splitter := NewSimpleSplitter()
	splitter.AddPool(Pool{Name: "a", Weight: 50, Balancer: poolA})
	splitter.AddPool(Pool{Name: "b", Weight: 50, Balancer: poolB})

	var wg sync.WaitGroup
	goroutines := 10
	iterationsPerGoroutine := 1000

	// Count results per goroutine, then aggregate
	// This avoids contention on a shared counter
	results := make([]map[string]int, goroutines)

	for g := 0; g < goroutines; g++ {
		results[g] = make(map[string]int)
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			for i := 0; i < iterationsPerGoroutine; i++ {
				_, poolName := splitter.Route()
				results[idx][poolName]++
			}
		}(g)
	}

	wg.Wait()

	// Aggregate counts
	total := make(map[string]int)
	for _, r := range results {
		for k, v := range r {
			total[k] += v
		}
	}

	// Verify distribution
	totalIterations := goroutines * iterationsPerGoroutine
	aRatio := float64(total["a"]) / float64(totalIterations)
	bRatio := float64(total["b"]) / float64(totalIterations)

	if math.Abs(aRatio-0.5) > 0.1 {
		t.Errorf("pool a ratio = %.2f, want ~0.5", aRatio)
	}
	if math.Abs(bRatio-0.5) > 0.1 {
		t.Errorf("pool b ratio = %.2f, want ~0.5", bRatio)
	}
}

func TestSplitterEmptyPools(t *testing.T) {
	splitter := NewSimpleSplitter()

	// Should return nil when no pools configured
	lb, name := splitter.Route()
	if lb != nil {
		t.Error("expected nil balancer for empty splitter")
	}
	if name != "" {
		t.Errorf("expected empty name, got %s", name)
	}
}

func TestSplitterZeroWeight(t *testing.T) {
	// Zero-weight pools should never be selected
	poolA := &mockLoadBalancer{name: "pool-a"}
	poolB := &mockLoadBalancer{name: "pool-b"}

	splitter := NewSimpleSplitter()
	splitter.AddPool(Pool{Name: "active", Weight: 100, Balancer: poolA})
	splitter.AddPool(Pool{Name: "disabled", Weight: 0, Balancer: poolB})

	// All requests should go to active pool
	for i := 0; i < 100; i++ {
		_, poolName := splitter.Route()
		if poolName == "disabled" {
			t.Error("zero-weight pool should never be selected")
		}
	}
}

func TestPoolInterface(t *testing.T) {
	// Verify Pool works with the loadbalancer.LoadBalancer interface
	var lb loadbalancer.LoadBalancer = &mockLoadBalancer{name: "test"}

	pool := Pool{
		Name:     "test-pool",
		Weight:   100,
		Balancer: lb,
	}

	if pool.Name != "test-pool" {
		t.Errorf("pool name = %s, want test-pool", pool.Name)
	}
	if pool.Weight != 100 {
		t.Errorf("pool weight = %d, want 100", pool.Weight)
	}
}
