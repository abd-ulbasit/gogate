// Package splitter implements traffic splitting for canary deployments and A/B testing.
//
// Traffic splitting allows routing a percentage of traffic to different backend pools.
// This is useful for:
//   - Canary deployments: Route 5% of traffic to new version, 95% to stable
//   - A/B testing: Split traffic between different feature variants
//   - Blue-green deployments: Gradually shift traffic from old to new
//
// Thread safety: All methods are safe for concurrent use.
package splitter

import (
	"errors"
	"math/rand"
	"sync"
	"sync/atomic"

	"gogate/internal/loadbalancer"
)

var (
	ErrNoTargets     = errors.New("no targets configured")
	ErrInvalidWeight = errors.New("weight must be positive")
)

// Target represents a backend pool with a weight for traffic splitting.
//
// Weight determines the proportion of traffic routed to this target.
// Example: Two targets with weights 90 and 10 get 90% and 10% of traffic.
//
// Why separate from Route?
//   - A single route can have multiple targets (canary deployment)
//   - Each target has its own load balancer (different backend pools)
//   - Cleaner separation: Route handles matching, Target handles distribution
type Target struct {
	Name string // Identifier for logging/metrics (e.g., "production", "canary")

	// LoadBalancer manages the backend pool for this target.
	// Why interface? Same LoadBalancer abstraction we use everywhere.
	LoadBalancer loadbalancer.LoadBalancer

	// Weight determines traffic proportion (relative to other targets).
	// Not a percentage - calculated as weight/sum(all weights).
	// Example: weights [90, 10] → 90% and 10%
	// Example: weights [3, 1] → 75% and 25%
	Weight int
}

// Splitter routes requests to different targets based on configured weights.
//
// Algorithm: Weighted random selection
//  1. Calculate total weight (sum of all target weights)
//  2. Generate random number in range [0, totalWeight)
//  3. Iterate through targets, subtracting weights until random < weight
//
// Why not round-robin weighted?
//   - Random is simpler and provides statistical fairness
//   - No state to track (no "current index")
//   - Each request is independent (good for distributed systems)
//   - Round-robin would require coordination in multi-instance deployments
//
// Example:
//
//	splitter := NewSplitter(
//	    &Target{Name: "prod", LoadBalancer: prodLB, Weight: 90},
//	    &Target{Name: "canary", LoadBalancer: canaryLB, Weight: 10},
//	)
//	target := splitter.Select() // 90% chance: prod, 10% chance: canary
type Splitter struct {
	targets     []*Target
	totalWeight int

	// Mutex protects targets during reconfiguration.
	// Read-heavy workload: Could use RWMutex but Select() is so fast
	// that the overhead of RWMutex isn't worth it.
	mu sync.RWMutex

	// Stats for observability
	selections map[string]*atomic.Int64 // Per-target selection counts
}

// NewSplitter creates a new traffic splitter with the given targets.
//
// At least one target with positive weight is required.
// Returns error if no valid targets provided.
func NewSplitter(targets ...*Target) (*Splitter, error) {
	if len(targets) == 0 {
		return nil, ErrNoTargets
	}

	s := &Splitter{
		targets:    make([]*Target, 0, len(targets)),
		selections: make(map[string]*atomic.Int64),
	}

	for _, t := range targets {
		if t.Weight <= 0 {
			return nil, ErrInvalidWeight
		}
		s.targets = append(s.targets, t)
		s.totalWeight += t.Weight
		s.selections[t.Name] = &atomic.Int64{}
	}

	return s, nil
}

// Select returns a target based on weighted random selection.
//
// Algorithm visualization:
//
//	Weights: [90, 10] → Total: 100
//	Number line: |-------- 90 --------|-10-|
//	Random 45 → hits "90" segment → returns first target
//	Random 95 → hits "10" segment → returns second target
//
// Time complexity: O(n) where n = number of targets
// For typical use (2-3 targets), this is fast enough.
func (s *Splitter) Select() *Target {
	s.mu.RLock()
	defer s.mu.RUnlock()

	if len(s.targets) == 1 {
		// Fast path: single target, no randomness needed
		s.selections[s.targets[0].Name].Add(1)
		return s.targets[0]
	}

	// Generate random number in [0, totalWeight)
	r := rand.Intn(s.totalWeight)

	// Walk through targets, subtracting weights until we find our bucket
	for _, t := range s.targets {
		r -= t.Weight
		if r < 0 {
			s.selections[t.Name].Add(1)
			return t
		}
	}

	// Should never reach here if weights are positive
	// Return last target as fallback
	last := s.targets[len(s.targets)-1]
	s.selections[last.Name].Add(1)
	return last
}

// Targets returns a copy of the configured targets.
//
// Returns copy to prevent external modification.
func (s *Splitter) Targets() []*Target {
	s.mu.RLock()
	defer s.mu.RUnlock()

	result := make([]*Target, len(s.targets))
	copy(result, s.targets)
	return result
}

// UpdateTargets replaces the current targets with new ones.
//
// This is useful for dynamic reconfiguration (e.g., increasing canary percentage).
// Thread-safe: Can be called while Select() is being used.
func (s *Splitter) UpdateTargets(targets ...*Target) error {
	if len(targets) == 0 {
		return ErrNoTargets
	}

	newTargets := make([]*Target, 0, len(targets))
	newTotalWeight := 0
	newSelections := make(map[string]*atomic.Int64)

	for _, t := range targets {
		if t.Weight <= 0 {
			return ErrInvalidWeight
		}
		newTargets = append(newTargets, t)
		newTotalWeight += t.Weight
		newSelections[t.Name] = &atomic.Int64{}
	}

	s.mu.Lock()
	s.targets = newTargets
	s.totalWeight = newTotalWeight
	s.selections = newSelections
	s.mu.Unlock()

	return nil
}

// Stats returns the selection count for each target.
//
// Useful for verifying traffic distribution matches configured weights.
// Returns a snapshot (values may change after return).
func (s *Splitter) Stats() map[string]int64 {
	s.mu.RLock()
	defer s.mu.RUnlock()

	result := make(map[string]int64, len(s.selections))
	for name, count := range s.selections {
		result[name] = count.Load()
	}
	return result
}

// =============================================================================
// Simplified Pool-based API (used by tests and simpler use cases)
// =============================================================================

// Pool represents a backend pool with routing configuration.
// This is a simpler wrapper around Target for common use cases.
type Pool struct {
	Name       string
	Weight     int
	Balancer   loadbalancer.LoadBalancer
	Predicates []func() bool // Optional: conditions for this pool
}

// SimpleSplitter provides a simplified API for traffic splitting.
// Use this when you don't need the full Target/Splitter complexity.
type SimpleSplitter struct {
	pools       []Pool
	totalWeight int
	counter     atomic.Uint64 // For deterministic round-robin distribution
	mu          sync.RWMutex
}

// NewSimpleSplitter creates a new simple splitter.
func NewSimpleSplitter() *SimpleSplitter {
	return &SimpleSplitter{
		pools: make([]Pool, 0),
	}
}

// AddPool adds a pool to the splitter.
func (s *SimpleSplitter) AddPool(pool Pool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.pools = append(s.pools, pool)
	s.totalWeight += pool.Weight
}

// Route returns the load balancer and pool name for the next request.
// Uses deterministic round-robin based on weights for predictable distribution.
func (s *SimpleSplitter) Route() (loadbalancer.LoadBalancer, string) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	if len(s.pools) == 0 || s.totalWeight == 0 {
		return nil, ""
	}

	// Use counter for deterministic distribution
	// This gives exact weight-based distribution over time
	count := s.counter.Add(1)
	position := int(count % uint64(s.totalWeight))

	// Find which pool this position falls into
	cumulative := 0
	for _, pool := range s.pools {
		cumulative += pool.Weight
		if position < cumulative {
			return pool.Balancer, pool.Name
		}
	}

	// Fallback to last pool
	last := s.pools[len(s.pools)-1]
	return last.Balancer, last.Name
}

// Alias for backward compatibility with tests
func NewSplitter2() *SimpleSplitter {
	return NewSimpleSplitter()
}
