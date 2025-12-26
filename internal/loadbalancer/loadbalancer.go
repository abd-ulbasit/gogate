package loadbalancer

import (
	"errors"
	"math"
	"sync"
	"sync/atomic"

	"gogate/internal/backend"
)

// LoadBalancer defines the interface for selecting a backend from a pool.
//
// Why interface?
// 1. Strategy pattern - swap algorithms without changing proxy code
// 2. Testability - mock in tests
// 3. Extensibility - add new algorithms (random, weighted, IP hash, etc.)
//
// Thread safety: All implementations MUST be safe for concurrent use.
type LoadBalancer interface {
	// Next returns the next healthy backend to use.
	// Returns nil, ErrNoHealthyBackends if all backends are down.
	Next() (*backend.Backend, error)

	// Backends returns all backends (for health checking, metrics, etc.)
	Backends() []*backend.Backend

	// AddBackend adds a backend to the pool (for dynamic configuration)
	AddBackend(b *backend.Backend)

	// RemoveBackend removes a backend from the pool
	RemoveBackend(addr string) bool

	// UpdateBackends replaces all backends with a new set (for hot reload)
	UpdateBackends(backends []*backend.Backend)
}

// Common errors
var (
	ErrNoHealthyBackends = errors.New("no healthy backends available")
	ErrNoBackends        = errors.New("no backends configured")
)

// RoundRobin distributes requests evenly across all healthy backends.
//
// Algorithm:
// 1. Keep a counter that increments on each request
// 2. Select backend at index: counter % len(backends)
// 3. Skip unhealthy backends (find next healthy one)
//
// Pros:
// - Simple to understand and implement
// - Fair distribution when backends have equal capacity
// - Stateless (no need to track connection counts)
//
// Cons:
// - Doesn't account for backend capacity differences
// - Doesn't account for current load (slow backend still gets traffic)
//
// Best for: Homogeneous backend pools with similar capacity
type RoundRobin struct {
	backends []*backend.Backend
	counter  atomic.Uint64
	mu       sync.RWMutex
}

// NewRoundRobin creates a new round-robin load balancer with the given backends.
func NewRoundRobin(backends []*backend.Backend) *RoundRobin {
	return &RoundRobin{
		backends: backends,
	}
}

// Next returns the next healthy backend in round-robin order.
func (rr *RoundRobin) Next() (*backend.Backend, error) {
	rr.mu.RLock()
	defer rr.mu.RUnlock()

	if len(rr.backends) == 0 {
		return nil, ErrNoBackends
	}

	count := rr.counter.Add(1)
	startIdx := int(count % uint64(len(rr.backends)))

	for i := 0; i < len(rr.backends); i++ {
		idx := (startIdx + i) % len(rr.backends)
		b := rr.backends[idx]
		if b.IsHealthy() {
			return b, nil
		}
	}

	return nil, ErrNoHealthyBackends
}

// Backends returns all backends.
func (rr *RoundRobin) Backends() []*backend.Backend {
	rr.mu.RLock()
	defer rr.mu.RUnlock()
	result := make([]*backend.Backend, len(rr.backends))
	copy(result, rr.backends)
	return result
}

// AddBackend adds a backend to the pool.
func (rr *RoundRobin) AddBackend(b *backend.Backend) {
	rr.mu.Lock()
	defer rr.mu.Unlock()
	rr.backends = append(rr.backends, b)
}

// RemoveBackend removes a backend by address.
func (rr *RoundRobin) RemoveBackend(addr string) bool {
	rr.mu.Lock()
	defer rr.mu.Unlock()

	for i, b := range rr.backends {
		if b.Addr() == addr {
			rr.backends[i] = rr.backends[len(rr.backends)-1]
			rr.backends = rr.backends[:len(rr.backends)-1]
			return true
		}
	}
	return false
}

// UpdateBackends replaces all backends with a new set.
func (rr *RoundRobin) UpdateBackends(backends []*backend.Backend) {
	rr.mu.Lock()
	defer rr.mu.Unlock()
	rr.backends = backends
}

// LeastConnections routes to the backend with fewest active connections.
//
// Algorithm:
// 1. Iterate through all healthy backends
// 2. Track the one with lowest ActiveConnections()
// 3. Return that backend
//
// Pros:
// - Naturally adapts to slow backends (they accumulate connections)
// - Better distribution under variable load
// - Works well with mixed-capacity backends
//
// Cons:
// - More overhead (must check all backends each time)
// - Doesn't account for connection duration
// - New backends may get thundering herd
//
// Best for: Mixed-capacity backends, long-lived connections, variable processing times
type LeastConnections struct {
	backends []*backend.Backend
	mu       sync.RWMutex
}

// NewLeastConnections creates a new least-connections load balancer.
func NewLeastConnections(backends []*backend.Backend) *LeastConnections {
	return &LeastConnections{
		backends: backends,
	}
}

// Next returns the healthy backend with the fewest active connections.
func (lc *LeastConnections) Next() (*backend.Backend, error) {
	lc.mu.RLock()
	defer lc.mu.RUnlock()

	if len(lc.backends) == 0 {
		return nil, ErrNoBackends
	}

	var selected *backend.Backend
	minConns := int64(math.MaxInt64)

	for _, b := range lc.backends {
		if !b.IsHealthy() {
			continue
		}

		conns := b.ActiveConnections()
		if conns < minConns {
			minConns = conns
			selected = b
		}
	}

	if selected == nil {
		return nil, ErrNoHealthyBackends
	}

	return selected, nil
}

// Backends returns all backends.
func (lc *LeastConnections) Backends() []*backend.Backend {
	lc.mu.RLock()
	defer lc.mu.RUnlock()
	result := make([]*backend.Backend, len(lc.backends))
	copy(result, lc.backends)
	return result
}

// AddBackend adds a backend to the pool.
func (lc *LeastConnections) AddBackend(b *backend.Backend) {
	lc.mu.Lock()
	defer lc.mu.Unlock()
	lc.backends = append(lc.backends, b)
}

// RemoveBackend removes a backend by address.
func (lc *LeastConnections) RemoveBackend(addr string) bool {
	lc.mu.Lock()
	defer lc.mu.Unlock()

	for i, b := range lc.backends {
		if b.Addr() == addr {
			lc.backends[i] = lc.backends[len(lc.backends)-1]
			lc.backends = lc.backends[:len(lc.backends)-1]
			return true
		}
	}
	return false
}

// UpdateBackends replaces all backends with a new set.
func (lc *LeastConnections) UpdateBackends(backends []*backend.Backend) {
	lc.mu.Lock()
	defer lc.mu.Unlock()
	lc.backends = backends
}

var _ LoadBalancer = (*RoundRobin)(nil)
var _ LoadBalancer = (*LeastConnections)(nil)
var _ LoadBalancer = (*WeightedRoundRobin)(nil)

// WeightedRoundRobin distributes requests based on backend weights.
//
// Algorithm: NGINX Smooth Weighted Round-Robin
// - Each backend has a current weight (starts at 0)
// - On each request:
//  1. Add effective weight to current weight for each backend
//  2. Select backend with highest current weight
//  3. Subtract total weight from selected backend's current weight
//
// This produces smooth distribution: with weights [5, 1, 1], sequence is
// A, A, B, A, A, C, A (not AAAAA B C)
//
// Pros:
// - Smooth distribution (avoids bursts to one backend)
// - Respects backend capacity differences
// - Used by NGINX, HAProxy
//
// Cons:
// - Slightly more complex than simple weighted
// - More state to track
//
// Best for: Heterogeneous backends with different capacities
type WeightedRoundRobin struct {
	mu       sync.Mutex
	backends []*weightedBackend
}

// weightedBackend wraps a backend with weight tracking for smooth WRR.
type weightedBackend struct {
	backend         *backend.Backend
	effectiveWeight int // Configured weight (can be reduced on failures)
	currentWeight   int // Running weight for selection algorithm
}

// NewWeightedRoundRobin creates a new weighted round-robin load balancer.
func NewWeightedRoundRobin(backends []*backend.Backend) *WeightedRoundRobin {
	wb := make([]*weightedBackend, len(backends))
	for i, b := range backends {
		wb[i] = &weightedBackend{
			backend:         b,
			effectiveWeight: b.Weight(),
			currentWeight:   0,
		}
	}
	return &WeightedRoundRobin{backends: wb}
}

// Next returns the next backend using smooth weighted round-robin.
func (wrr *WeightedRoundRobin) Next() (*backend.Backend, error) {
	wrr.mu.Lock()
	defer wrr.mu.Unlock()

	if len(wrr.backends) == 0 {
		return nil, ErrNoBackends
	}

	var selected *weightedBackend
	totalWeight := 0

	// Step 1: Add effective weight to current weight, track total
	for _, wb := range wrr.backends {
		if !wb.backend.IsHealthy() {
			continue
		}
		wb.currentWeight += wb.effectiveWeight
		totalWeight += wb.effectiveWeight

		// Step 2: Select backend with highest current weight
		if selected == nil || wb.currentWeight > selected.currentWeight {
			selected = wb
		}
	}

	if selected == nil {
		return nil, ErrNoHealthyBackends
	}

	// Step 3: Subtract total weight from selected backend
	selected.currentWeight -= totalWeight

	return selected.backend, nil
}

// Backends returns all backends.
func (wrr *WeightedRoundRobin) Backends() []*backend.Backend {
	wrr.mu.Lock()
	defer wrr.mu.Unlock()
	result := make([]*backend.Backend, len(wrr.backends))
	for i, wb := range wrr.backends {
		result[i] = wb.backend
	}
	return result
}

// AddBackend adds a backend to the pool.
func (wrr *WeightedRoundRobin) AddBackend(b *backend.Backend) {
	wrr.mu.Lock()
	defer wrr.mu.Unlock()
	wrr.backends = append(wrr.backends, &weightedBackend{
		backend:         b,
		effectiveWeight: b.Weight(),
		currentWeight:   0,
	})
}

// RemoveBackend removes a backend by address.
func (wrr *WeightedRoundRobin) RemoveBackend(addr string) bool {
	wrr.mu.Lock()
	defer wrr.mu.Unlock()

	for i, wb := range wrr.backends {
		if wb.backend.Addr() == addr {
			wrr.backends[i] = wrr.backends[len(wrr.backends)-1]
			wrr.backends = wrr.backends[:len(wrr.backends)-1]
			return true
		}
	}
	return false
}

// UpdateBackends replaces all backends with a new set.
func (wrr *WeightedRoundRobin) UpdateBackends(backends []*backend.Backend) {
	wrr.mu.Lock()
	defer wrr.mu.Unlock()

	wb := make([]*weightedBackend, len(backends))
	for i, b := range backends {
		wb[i] = &weightedBackend{
			backend:         b,
			effectiveWeight: b.Weight(),
			currentWeight:   0,
		}
	}
	wrr.backends = wb
}
