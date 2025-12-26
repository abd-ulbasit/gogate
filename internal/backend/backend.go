package backend

import (
	"context"
	"errors"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"time"
)

// Backend represents a single backend server that can receive proxied connections.
//
// Design decisions:
// 1. Why interface? Allows mocking in tests, different backend types (TCP, HTTP, gRPC)
// 2. Why embed health state? Tightly coupled - unhealthy backend shouldn't receive traffic
// 3. Why atomic for counters? Lock-free reads for hot path (connection counting)
//
// Thread safety: All methods are safe for concurrent use.
type Backend struct {
	// Immutable after creation
	addr   string // Backend address (host:port)
	weight int    // Weight for weighted load balancing (higher = more traffic)

	// Health state (protected by mutex)
	mu        sync.RWMutex
	healthy   bool      // Current health status
	lastCheck time.Time // When was health last checked
	lastError error     // Last error (for debugging)

	// Connection tracking (atomic for lock-free reads)
	activeConns atomic.Int64 // Current active connections to this backend
	totalConns  atomic.Int64 // Total connections ever made (for metrics)
	failures    atomic.Int64 // Total connection failures (for passive health)

	// Optional connection pooling (Milestone 7)
	pool *connPool
}

// NewBackend creates a new backend with the given address.
//
// Initially marked healthy - health checker will update based on actual checks.
// Weight defaults to 1 (equal distribution).
func NewBackend(addr string) *Backend {
	return &Backend{
		addr:    addr,
		weight:  1,
		healthy: true,
		pool:    nil, // pooling is opt-in via EnablePooling
	}
}

// NewBackendWithWeight creates a backend with custom weight.
//
// Weight affects load balancing: backend with weight 2 gets 2x traffic of weight 1.
// Use for: different server capacities, canary deployments, A/B testing.
func NewBackendWithWeight(addr string, weight int) *Backend {
	if weight < 1 {
		weight = 1
	}
	return &Backend{
		addr:    addr,
		weight:  weight,
		healthy: true,
		pool:    nil, // pooling is opt-in via EnablePooling
	}
}

// EnablePooling allows overriding the default pool configuration.
// Pass nil to disable pooling.
// EnablePooling enables TCP connection pooling for this backend.
// Pass a non-nil PoolConfig to enable pooling with the specified settings.
// Pass nil to explicitly disable pooling (connections will not be reused).
//
// When pooling is enabled:
//   - Dial() returns pooledConn which auto-returns to pool on Close()
//   - Connections are validated via liveness check before reuse
//   - Expired connections (TTL/idle timeout) are discarded
//
// Example:
//
//	backend.EnablePooling(&PoolConfig{MaxIdle: 10, IdleTimeout: 30*time.Second})
//	backend.EnablePooling(nil) // Disable pooling
func (b *Backend) EnablePooling(cfg *PoolConfig) {
	if cfg == nil {
		// Explicitly disable pooling - all connections close immediately
		b.pool = nil
		return
	}
	// Defensive copy of config to prevent external mutation
	copy := *cfg
	b.pool = newConnPool(copy)
}

// Addr returns the backend's address.
func (b *Backend) Addr() string {
	return b.addr
}

// Weight returns the backend's weight for load balancing.
func (b *Backend) Weight() int {
	return b.weight
}

// IsHealthy returns whether the backend is currently considered healthy.
//
// Thread-safe: Uses RWMutex (multiple concurrent readers allowed).
func (b *Backend) IsHealthy() bool {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return b.healthy
}

// SetHealthy updates the backend's health status.
//
// Called by:
// - Active health checker (periodic probes)
// - Passive health checker (connection failures)
func (b *Backend) SetHealthy(healthy bool, err error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.healthy = healthy
	b.lastCheck = time.Now()
	b.lastError = err
}

// LastError returns the last error that caused unhealthy status.
func (b *Backend) LastError() error {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return b.lastError
}

// LastCheck returns when health was last checked.
func (b *Backend) LastCheck() time.Time {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return b.lastCheck
}

// ActiveConnections returns current active connections to this backend.
//
// Used by: least-connections load balancer
// Thread-safe: Atomic read, no lock needed.
func (b *Backend) ActiveConnections() int64 {
	return b.activeConns.Load()
}

// TotalConnections returns total connections ever made to this backend.
func (b *Backend) TotalConnections() int64 {
	return b.totalConns.Load()
}

// Failures returns total connection failures to this backend.
func (b *Backend) Failures() int64 {
	return b.failures.Load()
}

// PoolStats returns connection pool statistics for this backend.
// Returns zero values if pooling is not enabled.
func (b *Backend) PoolStats() PoolStats {
	if b.pool == nil {
		return PoolStats{}
	}
	return b.pool.Stats()
}

// Dial establishes a TCP connection to the backend.
//
// This method handles:
// 1. Connection counting (increment before dial, decrement on close)
// 2. Failure tracking (for passive health checks)
// 3. Timeout handling (configurable via context)
//
// Returns a wrapped connection that automatically decrements counter on Close().
func (b *Backend) Dial(ctx context.Context) (net.Conn, error) {
	// Try pooled connection first
	if b.pool != nil {
		if conn := b.pool.get(); conn != nil {
			b.activeConns.Add(1)
			return conn, nil
		}
	}

	// Dial new connection
	b.totalConns.Add(1)
	dialer := &net.Dialer{Timeout: 10 * time.Second}
	if deadline, ok := ctx.Deadline(); ok {
		dialer.Timeout = time.Until(deadline)
	}

	conn, err := dialer.DialContext(ctx, "tcp", b.addr)
	if err != nil {
		b.failures.Add(1)
		return nil, err
	}

	pc := &pooledConn{
		Conn:      conn,
		backend:   b,
		pool:      b.pool,
		createdAt: time.Now(),
	}
	pc.touch()
	b.activeConns.Add(1)
	return pc, nil
}

// PoolConfig configures TCP connection pooling behavior.
type PoolConfig struct {
	MaxIdle     int           // maximum idle connections retained
	IdleTimeout time.Duration // close if idle for longer
	MaxLifetime time.Duration // close if older than this regardless of use
}

// DefaultPoolConfig returns conservative defaults suitable for most cases.
func DefaultPoolConfig() PoolConfig {
	return PoolConfig{
		MaxIdle:     64,
		IdleTimeout: 30 * time.Second,
		MaxLifetime: 2 * time.Minute,
	}
}

// connPool manages a pool of idle TCP connections for reuse.
//
// Why channel-based pool?
//   - Non-blocking put (if full, just close the connection)
//   - Non-blocking get (if empty, return nil and let caller dial)
//   - Simple, no mutex needed for the pool itself
//
// Metrics tracked:
//   - Hits: connections successfully retrieved from pool
//   - Misses: pool was empty, had to dial new connection
//   - Returned: connections successfully returned to pool
//   - Dropped: connections discarded (pool full, expired, or unusable)
type connPool struct {
	idle        chan *pooledConn
	idleTimeout time.Duration
	maxLifetime time.Duration

	// Pool metrics (atomic for lock-free reads)
	hits     atomic.Int64 // Successful pool retrievals
	misses   atomic.Int64 // Pool empty, had to dial
	returned atomic.Int64 // Successful returns to pool
	dropped  atomic.Int64 // Connections discarded (full/expired/unusable)
}

func newConnPool(cfg PoolConfig) *connPool {
	// Apply defaults for any unset values
	if cfg.MaxIdle <= 0 {
		cfg.MaxIdle = DefaultPoolConfig().MaxIdle
	}
	if cfg.IdleTimeout <= 0 {
		cfg.IdleTimeout = DefaultPoolConfig().IdleTimeout
	}
	if cfg.MaxLifetime <= 0 {
		cfg.MaxLifetime = DefaultPoolConfig().MaxLifetime
	}
	return &connPool{
		idle:        make(chan *pooledConn, cfg.MaxIdle),
		idleTimeout: cfg.IdleTimeout,
		maxLifetime: cfg.MaxLifetime,
		// Metrics start at zero (atomic.Int64 zero value)
	}
}

// get retrieves a reusable connection from the pool, or nil if none is available.
// Non-blocking: returns immediately if pool is empty.
//
// Metrics: Increments hits on success, misses on empty pool, dropped on expired/unusable.
func (p *connPool) get() net.Conn {
	if p == nil {
		return nil
	}
	now := time.Now()
	for {
		select {
		case conn := <-p.idle:
			if conn == nil {
				p.misses.Add(1)
				return nil
			}
			// Check if connection has expired (TTL or idle timeout exceeded)
			if conn.expired(now, p.idleTimeout, p.maxLifetime) {
				p.dropped.Add(1)
				conn.Conn.Close()
				continue
			}
			// Liveness check: ensure connection wasn't closed by peer while idle
			if !conn.usable() {
				p.dropped.Add(1)
				conn.Conn.Close()
				continue
			}
			// Valid connection - track hit and return
			p.hits.Add(1)
			conn.reuse(now)
			return conn
		default:
			// Pool empty - caller must dial new connection
			p.misses.Add(1)
			return nil
		}
	}
}

// put returns a connection to the pool for reuse.
// Returns true if successfully pooled, false if connection was dropped.
//
// Metrics: Increments returned on success, dropped if pool full or conn expired.
func (p *connPool) put(conn *pooledConn) bool {
	if p == nil || conn == nil {
		return false
	}
	now := time.Now()
	// Don't pool expired connections
	if conn.expired(now, p.idleTimeout, p.maxLifetime) {
		p.dropped.Add(1)
		conn.Conn.Close()
		return false
	}
	// Try non-blocking send to pool
	select {
	case p.idle <- conn:
		p.returned.Add(1)
		return true
	default:
		// Pool full - drop connection
		p.dropped.Add(1)
		conn.Conn.Close()
		return false
	}
}

// PoolStats returns current pool metrics snapshot.
type PoolStats struct {
	Hits     int64 `json:"hits"`
	Misses   int64 `json:"misses"`
	Returned int64 `json:"returned"`
	Dropped  int64 `json:"dropped"`
	Size     int   `json:"size"` // Current number of idle connections
}

// Stats returns the current pool metrics.
func (p *connPool) Stats() PoolStats {
	if p == nil {
		return PoolStats{}
	}
	return PoolStats{
		Hits:     p.hits.Load(),
		Misses:   p.misses.Load(),
		Returned: p.returned.Load(),
		Dropped:  p.dropped.Load(),
		Size:     len(p.idle),
	}
}

// connPool is a simple channel-backed idle pool per backend.
// It is safe for concurrent use.
// type // connPool manages a pool of idle TCP connections for reuse.
//
// Why channel-based pool?
//   - Non-blocking put (if full, just close the connection)
//   - Non-blocking get (if empty, return nil and let caller dial)
//   - Simple, no mutex needed for the pool itself
//
// Metrics tracked:
//   - Hits: connections successfully retrieved from pool
//   - Misses: pool was empty, had to dial new connection
//   - Returned: connections successfully returned to pool
//   - Dropped: connections discarded (pool full, expired, or unusable)
//
// pooledConn wraps net.Conn to support pool return and active counter tracking.
type pooledConn struct {
	net.Conn
	backend    *Backend
	pool       *connPool
	createdAt  time.Time
	lastUsed   atomic.Int64 // unix nanos
	returned   atomic.Bool
	halfClosed atomic.Bool // true if CloseWrite() was called
}

func (c *pooledConn) touch() {
	c.lastUsed.Store(time.Now().UnixNano())
	c.returned.Store(false)
}

func (c *pooledConn) reuse(now time.Time) {
	c.lastUsed.Store(now.UnixNano())
	c.returned.Store(false)
}

// usable performs a lightweight liveness check to avoid handing out
// connections that were closed by the backend while idle. For TCP we set a
// zero deadline and attempt a 1-byte read:
//   - Timeout -> still idle/usable
//   - EOF/closed -> drop
//   - Data read -> drop (avoid consuming protocol bytes)
//   - Other errors -> drop
func (c *pooledConn) usable() bool {
	tcpConn, ok := c.Conn.(*net.TCPConn)
	if !ok {
		return true
	}

	now := time.Now()
	if err := tcpConn.SetReadDeadline(now); err != nil {
		return false
	}

	var buf [1]byte
	_, err := tcpConn.Read(buf[:])

	// Clear deadline for future use
	_ = tcpConn.SetReadDeadline(time.Time{})

	if err == nil {
		// Unexpected data present; safer to drop
		return false
	}
	if ne, ok := err.(net.Error); ok && ne.Timeout() {
		return true
	}
	if errors.Is(err, io.EOF) || errors.Is(err, net.ErrClosed) {
		return false
	}
	return false
}

func (c *pooledConn) expired(now time.Time, idleTimeout, maxLifetime time.Duration) bool {
	if maxLifetime > 0 && now.Sub(c.createdAt) > maxLifetime {
		return true
	}
	if idleTimeout > 0 {
		last := time.Unix(0, c.lastUsed.Load())
		if now.Sub(last) > idleTimeout {
			return true
		}
	}
	return false
}

func (c *pooledConn) Close() error {
	if !c.returned.CompareAndSwap(false, true) {
		return nil // already returned
	}

	// Decrement active usage for this checkout
	c.backend.activeConns.Add(-1)

	if c.pool == nil {
		return c.Conn.Close()
	}

	// Don't pool half-closed connections (CloseWrite was called)
	// This prevents reusing connections that already sent FIN
	if c.halfClosed.Load() {
		return c.Conn.Close()
	}

	if c.pool.put(c) {
		return nil
	}
	return c.Conn.Close()
}

// CloseWrite supports half-close semantics.
// CloseWrite sends a FIN to the peer, signaling no more data will be sent.
// This enables half-close semantics for graceful connection termination.
//
// Guards against double-close: if the connection was already returned to the pool,
// CloseWrite is a no-op to prevent operating on a potentially reused connection.
//
// NOTE: Calling CloseWrite() marks the connection as half-closed, preventing
// it from being returned to the pool. This is correct because a connection that
// has sent FIN cannot be reused for a new request.
func (c *pooledConn) CloseWrite() error {
	// Guard: If already returned to pool, don't call CloseWrite on underlying conn.
	// The connection may have been reused by another goroutine.
	if c.returned.Load() {
		return nil
	}
	// Mark as half-closed so Close() won't return this to the pool
	c.halfClosed.Store(true)
	if tcpConn, ok := c.Conn.(*net.TCPConn); ok {
		return tcpConn.CloseWrite()
	}
	return nil
}

// HealthStatus returns a snapshot of the backend's health state.
type HealthStatus struct {
	Addr              string    `json:"addr"`
	Healthy           bool      `json:"healthy"`
	LastCheck         time.Time `json:"last_check"`
	LastError         string    `json:"last_error,omitempty"`
	ActiveConnections int64     `json:"active_connections"`
	TotalConnections  int64     `json:"total_connections"`
	Failures          int64     `json:"failures"`
}

func (b *Backend) HealthStatus() HealthStatus {
	b.mu.RLock()
	lastErr := ""
	if b.lastError != nil {
		lastErr = b.lastError.Error()
	}
	status := HealthStatus{
		Addr:              b.addr,
		Healthy:           b.healthy,
		LastCheck:         b.lastCheck,
		LastError:         lastErr,
		ActiveConnections: b.activeConns.Load(),
		TotalConnections:  b.totalConns.Load(),
		Failures:          b.failures.Load(),
	}
	b.mu.RUnlock()
	return status
}
