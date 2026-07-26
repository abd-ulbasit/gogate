package health

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/abd-ulbasit/sluice/internal/backend"
)

// Checker performs health checks on backends.
//
// Two types of health checking:
//
// 1. ACTIVE (this file): Periodically probe backends
//   - TCP connect check: is the port open?
//   - HTTP check: does HTTPPath return 2xx?
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

	// Shared HTTP client for CheckType "http", built on first use.
	httpOnce sync.Once
	http     *http.Client
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
	case "http":
		err = c.httpCheck(checkCtx, b.Addr())
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
//
// This says the port is open. It does not say the process behind it is serving:
// an application deadlocked after startup still completes the TCP handshake,
// because the kernel accepts on its behalf. Use httpCheck when the backend
// speaks HTTP.
func (c *Checker) tcpCheck(ctx context.Context, addr string) error {
	dialer := &net.Dialer{}
	conn, err := dialer.DialContext(ctx, "tcp", addr)
	if err != nil {
		return err
	}
	conn.Close()
	return nil
}

// httpCheck verifies the backend answers HTTPPath with a 2xx.
//
// A 2xx is required rather than "any response": a backend returning 503 from
// its health endpoint is telling us it is not ready, and treating that as
// healthy would defeat the point of asking. Redirects are not followed - a
// health endpoint that redirects is misconfigured, and following it could
// silently probe a different host.
//
// The response body is drained and discarded so the connection can be returned
// to the transport's idle pool; abandoning it unread forces a new TCP handshake
// on every interval.
func (c *Checker) httpCheck(ctx context.Context, addr string) error {
	path := c.config.HTTPPath
	if path == "" {
		path = "/health"
	}
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}

	url := "http://" + addr + path
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", "sluice-health/1")

	resp, err := c.httpClient().Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("health check %s returned %s", url, resp.Status)
	}
	return nil
}

// httpClient returns the shared client used for HTTP health checks.
//
// One client for all backends, created once: a fresh http.Client per check
// would discard the connection pool every interval and leak idle connections
// until the finalizer ran. Timeouts come from the per-check context, so the
// client itself sets none.
func (c *Checker) httpClient() *http.Client {
	c.httpOnce.Do(func() {
		c.http = &http.Client{
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
			Transport: &http.Transport{
				// Bounded to the number of backends we expect to probe.
				MaxIdleConns:        64,
				MaxIdleConnsPerHost: 2,
				IdleConnTimeout:     90 * time.Second,
				DisableCompression:  true,
			},
		}
	})
	return c.http
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
