package proxy

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"gogate/internal/circuitbreaker"
	"gogate/internal/loadbalancer"
	"gogate/internal/metrics"
	"gogate/internal/ratelimiter"
)

// TCPProxy is a Layer 4 (TCP) proxy that forwards raw bytes between client and backend.
//
// Design: Non-blocking Start() pattern (industry standard)
//
// Why non-blocking Start()?
// 1. Caller doesn't need to wrap in goroutine - less error-prone
// 2. Ready channel signals when actually accepting - deterministic, no time.Sleep hacks
// 3. Error channel exposes accept loop errors - better observability
// 4. Clear lifecycle: Start() initiates, Stop() completes, context cancels
//
// Used by: Kubernetes controller-runtime, NATS server, many production Go servers
//
// Lifecycle: Proxy is NOT restartable after Stop(). Create a new instance instead.
// This is intentional - simplifies state management and avoids subtle bugs.
//
// Key responsibilities:
// 1. Accept incoming TCP connections
// 2. Establish connection to backend
// 3. Copy data bidirectionally (client ↔ backend)
// 4. Handle connection lifecycle (half-close, cleanup)
// 5. Track active connections for graceful shutdown
type TCPProxy struct {
	listenAddr string // Address to listen on (e.g., ":8080")
	lb         loadbalancer.LoadBalancer
	logger     *slog.Logger // Structured logger

	// Lifecycle management
	listener    net.Listener   // TCP listener (nil until Start called)
	activeConns atomic.Int64   // Number of active connections (for observability)
	connWg      sync.WaitGroup // WaitGroup for active connections (for graceful shutdown)

	// Synchronization for lifecycle state
	mu       sync.Mutex     // Protects listener and running state
	running  bool           // True if accept loop is running
	stopped  bool           // True after Stop() called (proxy not restartable)
	ready    chan struct{}  // Closed when proxy is ready to accept connections
	done     chan struct{}  // Closed when proxy has fully stopped
	acceptWg sync.WaitGroup // WaitGroup for accept loop goroutine

	// Optional features (set via functional options)
	rateLimiter      *ratelimiter.TokenBucket                  // Global rate limiter (nil = no limit)
	circuitBreakers  map[string]*circuitbreaker.CircuitBreaker // Per-backend circuit breakers
	cbMu             sync.RWMutex                              // Protects circuitBreakers map
	cbConfig         circuitbreaker.Config                     // Config for new circuit breakers
	metricsCollector *metrics.Collector                        // Metrics collector (nil = no metrics)

	// Performance optimizations (Milestone 7)
	// Reuse copy buffers to reduce allocations during io.Copy-like operations.
	// Each buffer is 32KB to match stdlib defaults and typical TCP window sizes.
	bufPool sync.Pool
}

// Option configures optional TCPProxy features.
type Option func(*TCPProxy)

// WithRateLimiter adds global rate limiting.
// Requests exceeding the rate will be rejected immediately.
func WithRateLimiter(rl *ratelimiter.TokenBucket) Option {
	return func(p *TCPProxy) {
		p.rateLimiter = rl
	}
}

// WithCircuitBreaker enables per-backend circuit breakers.
// Each backend gets its own circuit breaker with the provided config.
func WithCircuitBreaker(config circuitbreaker.Config) Option {
	return func(p *TCPProxy) {
		p.circuitBreakers = make(map[string]*circuitbreaker.CircuitBreaker)
		p.cbConfig = config
	}
}

// WithMetrics enables metrics collection.
func WithMetrics(collector *metrics.Collector) Option {
	return func(p *TCPProxy) {
		p.metricsCollector = collector
	}
}

// NewTCPProxy creates a new TCP proxy instance.
//
// The proxy is created in stopped state. Call Start() to begin accepting connections.
// Use functional options to enable rate limiting, circuit breakers, and metrics.
//
// Example:
//
//	proxy := NewTCPProxy(addr, lb, logger,
//	    WithRateLimiter(ratelimiter.NewTokenBucket(100, 10)),
//	    WithCircuitBreaker(circuitbreaker.DefaultConfig()),
//	    WithMetrics(metrics.NewCollector()),
//	)
func NewTCPProxy(listenAddr string, lb loadbalancer.LoadBalancer, logger *slog.Logger, opts ...Option) *TCPProxy {
	p := &TCPProxy{
		listenAddr: listenAddr,
		lb:         lb,
		logger:     logger,
		ready:      make(chan struct{}),
		done:       make(chan struct{}),
	}
	// Initialize buffer pool for copy operations (Milestone 7)
	p.bufPool = sync.Pool{New: func() any {
		// Allocate 32KB buffers (same size used by io.Copy internally)
		b := make([]byte, 32*1024)
		return b
	}}
	for _, opt := range opts {
		opt(p)
	}
	return p
}

// getCircuitBreaker returns the circuit breaker for a backend, creating one if needed.
func (p *TCPProxy) getCircuitBreaker(addr string) *circuitbreaker.CircuitBreaker {
	if p.circuitBreakers == nil {
		return nil
	}

	p.cbMu.RLock()
	cb, ok := p.circuitBreakers[addr]
	p.cbMu.RUnlock()
	if ok {
		return cb
	}

	// Create new circuit breaker
	p.cbMu.Lock()
	defer p.cbMu.Unlock()
	// Double-check after acquiring write lock
	if cb, ok := p.circuitBreakers[addr]; ok {
		return cb
	}
	cb = circuitbreaker.New(p.cbConfig)
	p.circuitBreakers[addr] = cb
	return cb
}

// Start begins accepting connections. NON-BLOCKING - returns immediately.
//
// Lifecycle:
// 1. Creates TCP listener
// 2. Spawns accept loop goroutine
// 3. Returns immediately (caller doesn't need to wrap in goroutine)
// 4. Use Ready() channel to know when proxy is accepting
// 5. Use Stop() to gracefully shutdown
//
// Error handling:
// - Returns error only for listener creation failures (immediate errors)
// - Accept loop errors are logged, not returned (background operation)
// - Context cancellation triggers graceful shutdown
//
// Thread safety: Safe to call from any goroutine.
// Returns error if already running or if previously stopped (proxy is not restartable).
func (p *TCPProxy) Start(ctx context.Context) error {
	p.mu.Lock()
	defer p.mu.Unlock()

	// Check if proxy was previously stopped (not restartable)
	if p.stopped {
		return errors.New("proxy already stopped (create new instance to restart)")
	}

	if p.running {
		return errors.New("proxy already running")
	}
	if p.lb == nil {
		return errors.New("load balancer is required")
	}

	// Step 1: Create TCP listener
	var err error
	p.listener, err = net.Listen("tcp", p.listenAddr)
	if err != nil {
		return fmt.Errorf("failed to listen on %s: %w", p.listenAddr, err)
	}

	p.running = true

	// Step 2: Log startup (use actual bound address in case of :0)
	p.logger.Info("TCP proxy started", "listen", p.listener.Addr().String())

	// Step 3: Start accept loop in background goroutine
	p.acceptWg.Add(1)
	go p.acceptLoop(ctx)

	return nil
}

// acceptLoop runs the accept loop in a background goroutine.
// Separated from Start() for clarity and testability.
func (p *TCPProxy) acceptLoop(ctx context.Context) {
	defer p.acceptWg.Done()
	defer close(p.done) // Signal that proxy has fully stopped

	// Signal readiness AFTER goroutine starts (listener is already bound)
	close(p.ready)

	for {
		// Check for context cancellation (non-blocking)
		select {
		case <-ctx.Done():
			return
		default:
		}

		// Set accept deadline to allow periodic cancellation checks.
		// Accept() blocks, so without deadline we can't respond to ctx cancellation.
		// Type assertion is safe here - we know we created a TCP listener.
		if tcpListener, ok := p.listener.(*net.TCPListener); ok {
			if err := tcpListener.SetDeadline(time.Now().Add(1 * time.Second)); err != nil {
				// Deadline error usually means listener is closed - exit gracefully
				select {
				case <-ctx.Done():
					return
				default:
					p.logger.Debug("accept deadline error, listener likely closed", "error", err)
					return
				}
			}
		}

		conn, err := p.listener.Accept()
		if err != nil {
			// Check if it's a timeout (expected, for cancellation checking)
			var netErr net.Error
			if errors.As(err, &netErr) && netErr.Timeout() {
				continue // Normal timeout, check ctx and loop
			}

			// Check if shutdown was triggered
			select {
			case <-ctx.Done():
				return // Normal shutdown
			default:
				// Listener closed or fatal error - exit loop
				if !errors.Is(err, net.ErrClosed) {
					p.logger.Warn("accept error", "error", err)
				}
				return
			}
		}

		// Launch connection handler (non-blocking)
		p.connWg.Add(1)
		go p.handleConnection(conn)
	}
}

// Stop gracefully shuts down the proxy.
//
// Shutdown sequence:
// 1. Close listener (stop accepting new connections)
// 2. Wait for active connections to drain (with timeout from context)
// 3. Return when all connections are closed OR context deadline exceeded
//
// Thread safety: Safe to call from any goroutine. Safe to call multiple times.
// Idempotent: Calling Stop() on stopped proxy is a no-op.
//
// Note: After Stop(), the proxy cannot be restarted. Create a new instance.
func (p *TCPProxy) Stop(ctx context.Context) error {
	p.mu.Lock()
	if p.stopped {
		p.mu.Unlock()
		return nil // Already stopped, no-op (idempotent)
	}
	if !p.running {
		p.stopped = true
		p.mu.Unlock()
		return nil // Never started, mark stopped and return
	}
	p.running = false
	p.stopped = true

	// Step 1: Close listener (if exists)
	if p.listener != nil {
		if err := p.listener.Close(); err != nil {
			p.logger.Warn("error closing listener", "error", err)
		}
	}
	p.mu.Unlock()

	// Step 2: Wait for accept loop to exit
	p.acceptWg.Wait()

	// Step 3: Wait for active connections to drain (with timeout)
	done := make(chan struct{})
	go func() {
		p.connWg.Wait()
		close(done)
	}()

	select {
	case <-done:
		p.logger.Info("TCP proxy shutdown complete: all connections closed")
		return nil
	case <-ctx.Done():
		activeCount := p.activeConns.Load()
		if activeCount > 0 {
			return fmt.Errorf("shutdown timeout: %d connections still active", activeCount)
		}
		return nil
	}
}

// Ready returns a channel that is closed when the proxy is ready to accept connections.
//
// Usage:
//
//	proxy.Start(ctx)
//	<-proxy.Ready() // Block until proxy is accepting
//
// This replaces the error-prone time.Sleep pattern used in tests.
func (p *TCPProxy) Ready() <-chan struct{} {
	return p.ready
}

// Done returns a channel that is closed when the proxy has fully stopped.
//
// Usage:
//
//	proxy.Stop(ctx)
//	<-proxy.Done() // Block until proxy is fully stopped
func (p *TCPProxy) Done() <-chan struct{} {
	return p.done
}

// Addr returns the listener's address. Useful when listening on ":0".
// Returns empty string if not started.
func (p *TCPProxy) Addr() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.listener == nil {
		return ""
	}
	return p.listener.Addr().String()
}

// handleConnection proxies a single client connection to the backend.
//
// This is the core proxy logic. It must:
// 1. Check rate limit
// 2. Select backend and check circuit breaker
// 3. Connect to backend
// 4. Copy data bidirectionally (client ↔ backend)
// 5. Handle half-close properly
// 6. Clean up resources and record metrics
//
// Goroutine safety: Called from accept loop goroutine. Each call runs in its own goroutine.
// Resource cleanup: Uses defer for cleanup - connection closed even on panic.
func (p *TCPProxy) handleConnection(clientConn net.Conn) {
	startTime := time.Now()

	// Track connection (WaitGroup for graceful shutdown, atomic for metrics)
	defer p.connWg.Done()
	p.activeConns.Add(1)
	defer p.activeConns.Add(-1)

	// Always close client connection
	defer clientConn.Close()

	// Record connection metrics
	if p.metricsCollector != nil {
		p.metricsCollector.RecordConnection()
		defer func() {
			p.metricsCollector.RecordConnectionClosed(time.Since(startTime))
		}()
	}

	// Log connection start
	p.logger.Debug("new connection", "client", clientConn.RemoteAddr())

	// Step 0: Check global rate limit
	if p.rateLimiter != nil && !p.rateLimiter.Allow() {
		p.logger.Warn("rate limited", "client", clientConn.RemoteAddr())
		return // Connection will be closed by defer
	}

	// Step 1: Select backend via load balancer
	b, err := p.lb.Next()
	if err != nil {
		p.logger.Error("no backend available", "error", err)
		return
	}

	// Step 1.5: Check circuit breaker for this backend
	cb := p.getCircuitBreaker(b.Addr())
	if cb != nil {
		if err := cb.Allow(); err != nil {
			p.logger.Warn("circuit breaker open", "backend", b.Addr())
			return // Connection will be closed by defer
		}
	}

	// Step 2: Connect to backend
	backendConn, err := b.Dial(context.Background())
	if err != nil {
		p.logger.Error("failed to connect to backend", "backend", b.Addr(), "error", err)
		// Record failure in circuit breaker
		if cb != nil {
			cb.RecordResult(err)
		}
		return // Early return - can't proxy without backend
	}
	defer backendConn.Close()

	// Record success in circuit breaker (connection established)
	if cb != nil {
		cb.RecordResult(nil)
	}

	// Step 3: Set up bidirectional copy with byte counting
	var wg sync.WaitGroup
	wg.Add(2)

	var bytesSent, bytesReceived atomic.Int64

	// Client → Backend
	go func() {
		defer wg.Done()
		n, err := p.copyWithHalfCloseAndCount(backendConn, clientConn, "client→backend")
		bytesSent.Store(n)
		if err != nil {
			p.logger.Debug("error copying client to backend", "error", err)
		}
	}()

	// Backend → Client
	go func() {
		defer wg.Done()
		n, err := p.copyWithHalfCloseAndCount(clientConn, backendConn, "backend→client")
		bytesReceived.Store(n)
		if err != nil {
			p.logger.Debug("error copying backend to client", "error", err)
		}
	}()

	// Step 4: Wait for both directions to complete
	wg.Wait()

	// Step 5: Record request metrics
	if p.metricsCollector != nil {
		latency := time.Since(startTime)
		p.metricsCollector.RecordRequest(b.Addr(), true, latency, bytesSent.Load(), bytesReceived.Load())
	}

	// Step 6: Log connection end
	p.logger.Debug("connection closed",
		"client", clientConn.RemoteAddr(),
		"backend", b.Addr(),
		"bytes_sent", bytesSent.Load(),
		"bytes_received", bytesReceived.Load(),
		"duration", time.Since(startTime),
	)
}

// copyWithHalfCloseAndCount copies data and returns byte count.
// copyWithHalfCloseAndCount copies data between connections using pooled buffers.
//
// Uses sync.Pool to reuse 32KB buffers, reducing allocations on the hot path.
// After copy completes, sends FIN via CloseWrite for graceful half-close.
//
// Returns bytes copied and any error (EOF/closed errors are normalized to nil).
func (p *TCPProxy) copyWithHalfCloseAndCount(dst, src net.Conn, direction string) (int64, error) {
	// Get buffer from pool - New() guarantees non-nil []byte
	buf := p.bufPool.Get().([]byte)
	defer func() {
		// Reset slice to full capacity before returning to pool
		buf = buf[:cap(buf)]
		p.bufPool.Put(buf)
	}()

	// Copy data using pooled buffer
	n, err := io.CopyBuffer(dst, src, buf)

	// Half-close: signal end of write stream to peer
	// This is necessary for protocols that expect EOF/FIN after request data
	if cw, ok := dst.(interface{ CloseWrite() error }); ok {
		cw.CloseWrite()
	}

	// Normalize expected completion errors to nil
	if errors.Is(err, io.EOF) || errors.Is(err, net.ErrClosed) {
		return n, nil
	}
	return n, err
}

// ActiveConnections returns the number of currently active connections.
// Useful for metrics and graceful shutdown.
func (p *TCPProxy) ActiveConnections() int64 {
	return p.activeConns.Load()
}

// Notes for implementation:
//
// 1. Why two goroutines?
//    - TCP is full-duplex (both sides can send simultaneously)
//    - io.Copy blocks until EOF, so sequential copy won't work
//    - Example: If client sends data and waits for response,
//      sequential copy would deadlock
//
// 2. Why CloseWrite instead of Close?
//    - TCP supports half-close (FIN in one direction only)
//    - When client closes write side, it may still receive data
//    - Example: HTTP response after request body is fully sent
//
// 3. Connection lifecycle:
//    Client             Proxy               Backend
//      │ ─── SYN ────────►│                   │
//      │ ◄── SYN-ACK ─────│                   │
//      │ ─── ACK ────────►│                   │
//      │                  │─── SYN ──────────►│
//      │                  │◄── SYN-ACK ───────│
//      │                  │─── ACK ──────────►│
//      │                  │                   │
//      │═══════════════════╬═══════════════════│  Data flow
//      │                  │                   │
//      │─── FIN ─────────►│                   │  Client closes write
//      │                  │─── FIN ──────────►│
//      │                  │◄── data ──────────│  Backend still sending
//      │◄── data ─────────│                   │
//      │                  │◄── FIN ───────────│  Backend closes write
//      │◄── FIN ──────────│                   │
//
// 4. Error handling priorities:
//    - io.EOF = normal close (not an error)
//    - net.ErrClosed = connection already closed (expected during shutdown)
//    - syscall.ECONNRESET = peer reset connection (log but not critical)
//    - Other errors = log at warn/error level
//
// 5. Resource leaks to avoid:
//    - Forgetting defer conn.Close() → file descriptor leak
//    - Not cancelling goroutines → goroutine leak
//    - Not handling context cancellation → hanging on shutdown
