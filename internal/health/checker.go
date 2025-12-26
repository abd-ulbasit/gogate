package health

import (
	"context"
	"log/slog"
	"net"
	"sync"
	"time"

	"gogate/internal/backend"
)

// Checker performs health checks on backends.
//
// Two types of health checking:
//
// 1. ACTIVE (this file): Periodically probe backends
//   - TCP connect check (can we establish connection?)
//   - HTTP check (does /health return 200?) - TODO
//   - Custom check (user-defined) - TODO
//
// 2. PASSIVE (in proxy): Track connection failures
//   - If backend fails X connections in Y seconds, mark unhealthy
//   - Automatically recover when connections succeed
//
// Why both?
// - Active catches backends that are "up but not serving"
// - Passive catches issues faster (no need to wait for probe interval)
// - Combined approach is most reliable
type Checker struct {
	backends []*backend.Backend
	logger   *slog.Logger
	config   CheckerConfig

	mu           sync.RWMutex
	running      bool
	done         chan struct{}
	wg           sync.WaitGroup
	ready        chan struct{}
	successCount map[string]int
	failureCount map[string]int
}

// CheckerConfig contains health check configuration.
type CheckerConfig struct {
	// Interval between health checks (default: 5s)
	Interval time.Duration

	// Timeout for each health check (default: 2s)
	Timeout time.Duration

	// UnhealthyThreshold: how many consecutive failures before marking unhealthy (default: 3)
	UnhealthyThreshold int

	// HealthyThreshold: how many consecutive successes before marking healthy (default: 2)
	HealthyThreshold int

	// CheckType: "tcp" (default) or "http"
	CheckType string

	// HTTPPath: path to check for HTTP checks (default: "/health")
	HTTPPath string
}

// DefaultCheckerConfig returns sensible default configuration.
func DefaultCheckerConfig() CheckerConfig {
	return CheckerConfig{
		Interval:           5 * time.Second,
		Timeout:            2 * time.Second,
		UnhealthyThreshold: 3,
		HealthyThreshold:   2,
		CheckType:          "tcp",
		HTTPPath:           "/health",
	}
}

// NewChecker creates a new health checker.
//
// Pass the same backends slice used by the load balancer.
// Health updates are reflected immediately (shared pointers).
func NewChecker(backends []*backend.Backend, logger *slog.Logger, config CheckerConfig) *Checker {
	if config.Interval == 0 {
		config.Interval = 5 * time.Second
	}
	if config.Timeout == 0 {
		config.Timeout = 2 * time.Second
	}
	if config.UnhealthyThreshold == 0 {
		config.UnhealthyThreshold = 3
	}
	if config.HealthyThreshold == 0 {
		config.HealthyThreshold = 2
	}

	return &Checker{
		backends:     backends,
		logger:       logger,
		config:       config,
		done:         make(chan struct{}),
		ready:        make(chan struct{}),
		successCount: make(map[string]int),
		failureCount: make(map[string]int),
	}
}

// Start begins periodic health checking.
//
// Non-blocking: Returns immediately, checks run in background goroutine.
// Call Stop() to terminate.
func (c *Checker) Start(ctx context.Context) error {
	c.mu.Lock()
	if c.running {
		c.mu.Unlock()
		return nil
	}
	c.running = true
	c.mu.Unlock()

	c.logger.Info("health checker started",
		"interval", c.config.Interval,
		"timeout", c.config.Timeout,
		"backends", len(c.backends),
	)

	c.wg.Add(1)
	go c.checkLoop(ctx)
	close(c.ready)

	return nil
}

// Stop stops the health checker.
//
// Blocks until checker goroutine exits.
func (c *Checker) Stop() {
	c.mu.Lock()
	if !c.running {
		c.mu.Unlock()
		return
	}
	c.running = false
	c.mu.Unlock()

	close(c.done)
	c.wg.Wait()
	c.logger.Info("health checker stopped")
}

// checkLoop runs periodic health checks.
func (c *Checker) checkLoop(ctx context.Context) {
	defer c.wg.Done()

	ticker := time.NewTicker(c.config.Interval)
	defer ticker.Stop()

	// Run initial check immediately
	c.checkAll(ctx)

	for {
		select {
		case <-ctx.Done():
			return
		case <-c.done:
			return
		case <-ticker.C:
			c.checkAll(ctx)
		}
	}
}

// checkAll checks all backends concurrently.
func (c *Checker) checkAll(ctx context.Context) {
	var wg sync.WaitGroup
	for _, b := range c.backends {
		wg.Add(1)
		go func(b *backend.Backend) {
			defer wg.Done()
			c.checkOne(ctx, b)
		}(b)
	}
	wg.Wait()
}

// checkOne performs a single health check on one backend.
func (c *Checker) checkOne(ctx context.Context, b *backend.Backend) {
	checkCtx, cancel := context.WithTimeout(ctx, c.config.Timeout)
	defer cancel()

	var err error
	switch c.config.CheckType {
	case "tcp":
		err = c.tcpCheck(checkCtx, b.Addr())
	case "http":
		// TODO(basit): Implement HTTP health check
		err = c.tcpCheck(checkCtx, b.Addr())
	default:
		err = c.tcpCheck(checkCtx, b.Addr())
	}

	wasHealthy := b.IsHealthy()
	addr := b.Addr()

	c.mu.Lock()
	if err != nil {
		c.failureCount[addr]++
		c.successCount[addr] = 0
		if c.failureCount[addr] >= c.config.UnhealthyThreshold {
			c.mu.Unlock()
			b.SetHealthy(false, err)
			if wasHealthy {
				c.logger.Warn("backend became unhealthy",
					"backend", addr,
					"error", err,
				)
			}
			return
		}
		c.mu.Unlock()
		return
	}

	// success path
	c.failureCount[addr] = 0
	c.successCount[addr]++
	readyToMark := c.successCount[addr] >= c.config.HealthyThreshold
	b.SetHealthy(true, nil)
	c.mu.Unlock()

	if readyToMark && !wasHealthy {
		b.SetHealthy(true, nil)
		c.logger.Info("backend became healthy", "backend", addr)
	}
}

// tcpCheck verifies backend accepts TCP connections.
func (c *Checker) tcpCheck(ctx context.Context, addr string) error {
	dialer := &net.Dialer{}
	conn, err := dialer.DialContext(ctx, "tcp", addr)
	if err != nil {
		return err
	}
	conn.Close()
	return nil
}

// UpdateBackends replaces the backend list with a new set.
// Used for hot reload and service discovery updates.
func (c *Checker) UpdateBackends(backends []*backend.Backend) {
	c.mu.Lock()
	defer c.mu.Unlock()

	// Clear counters for removed backends, keep counters for existing ones
	newAddrs := make(map[string]bool)
	for _, b := range backends {
		newAddrs[b.Addr()] = true
	}

	// Remove counters for backends that no longer exist
	for addr := range c.successCount {
		if !newAddrs[addr] {
			delete(c.successCount, addr)
			delete(c.failureCount, addr)
		}
	}

	c.backends = backends
	c.logger.Info("health checker backends updated", "count", len(backends))
}
