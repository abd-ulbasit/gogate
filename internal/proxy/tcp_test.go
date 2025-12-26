package proxy

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"sync"
	"testing"
	"time"

	"gogate/internal/backend"
	"gogate/internal/circuitbreaker"
	"gogate/internal/loadbalancer"
	"gogate/internal/metrics"
	"gogate/internal/ratelimiter"
)

// TestTCPProxyBasicForwarding verifies data flows correctly through the proxy.
func TestTCPProxyBasicForwarding(t *testing.T) {
	// Start a simple echo backend
	bListener := startEchoServer(t)
	defer bListener.Close()

	backends := []*backend.Backend{backend.NewBackend(bListener.Addr().String())}
	lb := loadbalancer.NewRoundRobin(backends)

	// Start proxy pointing to backend
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	proxy := NewTCPProxy(":0", lb, logger)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Start proxy (non-blocking - returns immediately)
	if err := proxy.Start(ctx); err != nil {
		t.Fatalf("failed to start proxy: %v", err)
	}
	<-proxy.Ready() // Wait until actually accepting
	defer proxy.Stop(context.Background())

	// Connect to proxy using Addr() method
	client, err := net.Dial("tcp", proxy.Addr())
	if err != nil {
		t.Fatalf("failed to connect to proxy: %v", err)
	}
	defer client.Close()

	// Set read timeout to prevent hanging
	client.SetDeadline(time.Now().Add(5 * time.Second))

	// Test data flow: client → proxy → backend → proxy → client
	testMsg := "hello proxy"
	if _, err := client.Write([]byte(testMsg)); err != nil {
		t.Fatalf("failed to write to proxy: %v", err)
	}

	// Backend echoes, so we should receive it back
	buf := make([]byte, len(testMsg))
	if _, err := io.ReadFull(client, buf); err != nil {
		t.Fatalf("failed to read from proxy: %v", err)
	}

	if got := string(buf); got != testMsg {
		t.Errorf("expected %q, got %q", testMsg, got)
	}
}

// TestTCPProxyBidirectional verifies both directions work concurrently.
func TestTCPProxyBidirectional(t *testing.T) {
	// Start backend that reads and writes independently
	bListener := startBidirectionalServer(t)
	defer bListener.Close()

	lb := buildRoundRobin(bListener)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	proxy := NewTCPProxy(":0", lb, logger)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Start proxy (non-blocking)
	if err := proxy.Start(ctx); err != nil {
		t.Fatalf("failed to start proxy: %v", err)
	}
	<-proxy.Ready()
	defer proxy.Stop(context.Background())

	client, err := net.Dial("tcp", proxy.Addr())
	if err != nil {
		t.Fatalf("failed to connect: %v", err)
	}
	defer client.Close()
	client.SetDeadline(time.Now().Add(5 * time.Second))

	// Send multiple messages and verify responses come back
	for i := 0; i < 5; i++ {
		msg := fmt.Sprintf("message-%d", i)
		if _, err := client.Write([]byte(msg + "\n")); err != nil {
			t.Fatalf("write %d failed: %v", i, err)
		}

		buf := make([]byte, 128)
		n, err := client.Read(buf)
		if err != nil {
			t.Fatalf("read %d failed: %v", i, err)
		}

		if got := string(buf[:n]); got != "echo: "+msg+"\n" {
			t.Errorf("expected %q, got %q", "echo: "+msg+"\n", got)
		}
	}
}

// TestTCPProxyConcurrentConnections verifies multiple clients work simultaneously.
func TestTCPProxyConcurrentConnections(t *testing.T) {
	bListener := startEchoServer(t)
	defer bListener.Close()

	lb := buildRoundRobin(bListener)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	proxy := NewTCPProxy(":0", lb, logger)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Start proxy (non-blocking)
	if err := proxy.Start(ctx); err != nil {
		t.Fatalf("failed to start proxy: %v", err)
	}
	<-proxy.Ready()
	defer proxy.Stop(context.Background())

	// Launch 10 concurrent clients
	const numClients = 10
	var wg sync.WaitGroup
	errors := make(chan error, numClients)

	for i := 0; i < numClients; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()

			client, err := net.Dial("tcp", proxy.Addr())
			if err != nil {
				errors <- fmt.Errorf("client %d dial failed: %w", id, err)
				return
			}
			defer client.Close()

			msg := fmt.Sprintf("client-%d", id)
			if _, err := client.Write([]byte(msg)); err != nil {
				errors <- fmt.Errorf("client %d write failed: %w", id, err)
				return
			}

			buf := make([]byte, len(msg))
			if _, err := io.ReadFull(client, buf); err != nil {
				errors <- fmt.Errorf("client %d read failed: %w", id, err)
				return
			}

			if got := string(buf); got != msg {
				errors <- fmt.Errorf("client %d: expected %q, got %q", id, msg, got)
			}
		}(i)
	}

	wg.Wait()
	close(errors)

	// Check for any errors
	for err := range errors {
		t.Error(err)
	}

	// Verify active connection count drops back to 0 (allow brief delay)
	deadline := time.Now().Add(500 * time.Millisecond)
	for {
		if count := proxy.ActiveConnections(); count == 0 {
			break
		}
		if time.Now().After(deadline) {
			count := proxy.ActiveConnections()
			t.Fatalf("expected 0 active connections, got %d", count)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// TestTCPProxyGracefulShutdown verifies existing connections complete during shutdown.
func TestTCPProxyGracefulShutdown(t *testing.T) {
	bListener := startSlowEchoServer(t, 500*time.Millisecond)
	defer bListener.Close()

	lb := buildRoundRobin(bListener)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	proxy := NewTCPProxy(":0", lb, logger)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Start proxy (non-blocking)
	if err := proxy.Start(ctx); err != nil {
		t.Fatalf("failed to start proxy: %v", err)
	}
	<-proxy.Ready()

	// Connect client and start slow operation
	client, err := net.Dial("tcp", proxy.Addr())
	if err != nil {
		t.Fatalf("failed to connect: %v", err)
	}
	defer client.Close()

	// Send message (backend will delay response)
	msg := "slow-message"
	if _, err := client.Write([]byte(msg)); err != nil {
		t.Fatalf("write failed: %v", err)
	}
	if tc, ok := client.(*net.TCPConn); ok {
		tc.CloseWrite() // signal no more data, let backend respond and io.Copy exit
	}

	// Trigger shutdown while connection is active
	time.Sleep(50 * time.Millisecond) // Ensure message is in flight
	proxy.Stop(context.Background())

	// Connection should complete despite shutdown
	buf := make([]byte, len(msg))
	client.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, err := io.ReadFull(client, buf); err != nil {
		t.Fatalf("read after shutdown failed: %v", err)
	}

	if got := string(buf); got != msg {
		t.Errorf("expected %q, got %q", msg, got)
	}
}

// TestTCPProxyBackendFailure verifies proxy handles backend connection failures.
func TestTCPProxyBackendFailure(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	// Point to non-existent backend
	lb := loadbalancer.NewRoundRobin([]*backend.Backend{backend.NewBackend("127.0.0.1:1")})
	proxy := NewTCPProxy(":0", lb, logger)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Start proxy (non-blocking)
	if err := proxy.Start(ctx); err != nil {
		t.Fatalf("failed to start proxy: %v", err)
	}
	<-proxy.Ready()
	defer proxy.Stop(context.Background())

	// Connect should succeed (to proxy)
	client, err := net.Dial("tcp", proxy.Addr())
	if err != nil {
		t.Fatalf("failed to connect to proxy: %v", err)
	}
	defer client.Close()

	// But proxy can't reach backend, so connection should close
	client.SetReadDeadline(time.Now().Add(2 * time.Second))
	buf := make([]byte, 1)
	_, err = client.Read(buf)
	if err == nil {
		t.Error("expected read error after backend failure")
	}
}

// TestTCPProxyHalfClose verifies half-close semantics work correctly.
func TestTCPProxyHalfClose(t *testing.T) {
	bListener := startEchoServer(t)
	defer bListener.Close()

	lb := buildRoundRobin(bListener)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	proxy := NewTCPProxy(":0", lb, logger)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Start proxy (non-blocking)
	if err := proxy.Start(ctx); err != nil {
		t.Fatalf("failed to start proxy: %v", err)
	}
	<-proxy.Ready()
	defer proxy.Stop(context.Background())

	client, err := net.Dial("tcp", proxy.Addr())
	if err != nil {
		t.Fatalf("failed to connect: %v", err)
	}
	defer client.Close()

	msg := "test"
	if _, err := client.Write([]byte(msg)); err != nil {
		t.Fatalf("write failed: %v", err)
	}

	// Close write side (half-close)
	if conn, ok := client.(*net.TCPConn); ok {
		conn.CloseWrite()
	}

	// Should still be able to read response
	buf := make([]byte, len(msg))
	client.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, err := io.ReadFull(client, buf); err != nil {
		t.Fatalf("read after half-close failed: %v", err)
	}

	if got := string(buf); got != msg {
		t.Errorf("expected %q, got %q", msg, got)
	}
}

// TestTCPProxyActiveConnections verifies connection counting is accurate.
func TestTCPProxyActiveConnections(t *testing.T) {
	bListener := startEchoServer(t)
	defer bListener.Close()

	lb := buildRoundRobin(bListener)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	proxy := NewTCPProxy(":0", lb, logger)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Start proxy (non-blocking)
	if err := proxy.Start(ctx); err != nil {
		t.Fatalf("failed to start proxy: %v", err)
	}
	<-proxy.Ready()
	defer proxy.Stop(context.Background())

	if count := proxy.ActiveConnections(); count != 0 {
		t.Errorf("expected 0 active connections initially, got %d", count)
	}

	// Open 3 connections
	clients := make([]net.Conn, 3)
	for i := range clients {
		var err error
		clients[i], err = net.Dial("tcp", proxy.Addr())
		if err != nil {
			t.Fatalf("client %d dial failed: %v", i, err)
		}
		defer clients[i].Close()
	}

	// Give connections time to be registered
	time.Sleep(50 * time.Millisecond)

	if count := proxy.ActiveConnections(); count != 3 {
		t.Errorf("expected 3 active connections, got %d", count)
	}

	// Close 2 connections
	clients[0].Close()
	clients[1].Close()

	// Give time for cleanup
	time.Sleep(100 * time.Millisecond)

	if count := proxy.ActiveConnections(); count != 1 {
		t.Errorf("expected 1 active connection after closing 2, got %d", count)
	}
}

// TestTCPProxyLoadBalancerIntegration ensures multiple backends receive traffic.
func TestTCPProxyLoadBalancerIntegration(t *testing.T) {
	backend1 := startStaticServer(t, "one")
	defer backend1.Close()
	backend2 := startStaticServer(t, "two")
	defer backend2.Close()

	lb := buildRoundRobin(backend1, backend2)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	proxy := NewTCPProxy(":0", lb, logger)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := proxy.Start(ctx); err != nil {
		t.Fatalf("failed to start proxy: %v", err)
	}
	<-proxy.Ready()
	defer proxy.Stop(context.Background())

	responses := make(map[string]int)
	for i := 0; i < 6; i++ {
		client, err := net.Dial("tcp", proxy.Addr())
		if err != nil {
			t.Fatalf("dial proxy: %v", err)
		}
		client.SetDeadline(time.Now().Add(2 * time.Second))
		client.Write([]byte("ping"))
		buf := make([]byte, 16)
		n, err := client.Read(buf)
		client.Close()
		if err != nil {
			t.Fatalf("read: %v", err)
		}
		responses[string(buf[:n])]++
	}

	if len(responses) != 2 {
		t.Fatalf("expected responses from two backends, got %v", responses)
	}
}

// =============================================================================
// Week 4 Integration Tests: Rate Limiter, Circuit Breaker, Metrics
// =============================================================================

// TestTCPProxyWithRateLimiter verifies rate limiting blocks excess connections.
func TestTCPProxyWithRateLimiter(t *testing.T) {
	bListener := startEchoServer(t)
	defer bListener.Close()

	lb := buildRoundRobin(bListener)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	// Rate limit: 2 requests per second, burst of 2
	rl := ratelimiter.NewTokenBucket(2, 2)
	proxy := NewTCPProxy(":0", lb, logger, WithRateLimiter(rl))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := proxy.Start(ctx); err != nil {
		t.Fatalf("failed to start proxy: %v", err)
	}
	<-proxy.Ready()
	defer proxy.Stop(context.Background())

	// First 2 connections should succeed (burst)
	for i := 0; i < 2; i++ {
		client, err := net.Dial("tcp", proxy.Addr())
		if err != nil {
			t.Fatalf("connection %d failed: %v", i, err)
		}
		client.SetDeadline(time.Now().Add(1 * time.Second))
		client.Write([]byte("test"))
		buf := make([]byte, 4)
		_, err = io.ReadFull(client, buf)
		client.Close()
		if err != nil {
			t.Fatalf("connection %d read failed: %v", i, err)
		}
	}

	// Third connection should be rate limited (connection accepted but closed immediately)
	client, err := net.Dial("tcp", proxy.Addr())
	if err != nil {
		t.Fatalf("third dial failed: %v", err)
	}
	defer client.Close()
	client.SetDeadline(time.Now().Add(500 * time.Millisecond))

	// Try to send data - should fail because connection is closed by proxy
	_, err = client.Write([]byte("test"))
	if err == nil {
		// Write might succeed if buffered, but read should fail
		buf := make([]byte, 4)
		_, err = io.ReadFull(client, buf)
	}

	// We expect either write or read to fail because proxy closes rate-limited connections
	if err == nil {
		t.Error("expected rate-limited connection to fail")
	}

	// Verify rate limiter stats
	stats := rl.Stats()
	if stats.TotalAllowed < 2 {
		t.Errorf("expected at least 2 allowed, got %d", stats.TotalAllowed)
	}
	if stats.TotalDenied < 1 {
		t.Errorf("expected at least 1 denied, got %d", stats.TotalDenied)
	}
}

// TestTCPProxyWithCircuitBreaker verifies circuit breaker opens on failures.
func TestTCPProxyWithCircuitBreaker(t *testing.T) {
	// Start a backend that will be closed mid-test
	bListener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to start backend: %v", err)
	}
	backendAddr := bListener.Addr().String()
	bListener.Close() // Close immediately to simulate failure

	backends := []*backend.Backend{backend.NewBackend(backendAddr)}
	lb := loadbalancer.NewRoundRobin(backends)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	// Circuit breaker: opens after 2 failures
	cbConfig := circuitbreaker.Config{
		FailureThreshold: 2,
		SuccessThreshold: 1,
		Timeout:          100 * time.Millisecond,
		Window:           1 * time.Second,
	}
	proxy := NewTCPProxy(":0", lb, logger, WithCircuitBreaker(cbConfig))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := proxy.Start(ctx); err != nil {
		t.Fatalf("failed to start proxy: %v", err)
	}
	<-proxy.Ready()
	defer proxy.Stop(context.Background())

	// Make 3 connections - first 2 will fail (backend down) and trip circuit
	// Third should be rejected by circuit breaker
	for i := 0; i < 3; i++ {
		client, err := net.Dial("tcp", proxy.Addr())
		if err != nil {
			t.Logf("connection %d dial failed: %v", i, err)
			continue
		}
		client.SetDeadline(time.Now().Add(500 * time.Millisecond))
		client.Write([]byte("test"))
		buf := make([]byte, 4)
		io.ReadFull(client, buf) // Will fail
		client.Close()
	}

	// Verify circuit breaker is open
	cb := proxy.getCircuitBreaker(backendAddr)
	if cb == nil {
		t.Fatal("expected circuit breaker to be created")
	}
	if cb.State() != circuitbreaker.StateOpen {
		t.Errorf("expected circuit breaker to be OPEN, got %s", cb.State())
	}

	// Wait for timeout, then call Allow() to trigger transition to half-open
	time.Sleep(150 * time.Millisecond)
	// Circuit breaker transitions on Allow() call
	cb.Allow() // This should transition to half-open
	if cb.State() != circuitbreaker.StateHalfOpen {
		t.Errorf("expected circuit breaker to be HALF-OPEN after timeout, got %s", cb.State())
	}
}

// TestTCPProxyWithMetrics verifies metrics are recorded correctly.
func TestTCPProxyWithMetrics(t *testing.T) {
	bListener := startEchoServer(t)
	defer bListener.Close()

	lb := buildRoundRobin(bListener)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	collector := metrics.NewCollector()
	proxy := NewTCPProxy(":0", lb, logger, WithMetrics(collector))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := proxy.Start(ctx); err != nil {
		t.Fatalf("failed to start proxy: %v", err)
	}
	<-proxy.Ready()
	defer proxy.Stop(context.Background())

	// Make 3 connections
	for i := 0; i < 3; i++ {
		client, err := net.Dial("tcp", proxy.Addr())
		if err != nil {
			t.Fatalf("connection %d failed: %v", i, err)
		}
		client.SetDeadline(time.Now().Add(1 * time.Second))
		client.Write([]byte("hello"))
		buf := make([]byte, 5)
		io.ReadFull(client, buf)
		client.Close()
	}

	// Allow metrics to settle
	time.Sleep(100 * time.Millisecond)

	// Verify metrics
	snapshot := collector.Snapshot()

	if snapshot.ConnectionsTotal < 3 {
		t.Errorf("expected at least 3 total connections, got %d", snapshot.ConnectionsTotal)
	}

	if snapshot.RequestsTotal < 3 {
		t.Errorf("expected at least 3 requests, got %d", snapshot.RequestsTotal)
	}

	// Check backend metrics
	backendAddr := bListener.Addr().String()
	if bm, ok := snapshot.Backends[backendAddr]; ok {
		if bm.RequestsTotal < 3 {
			t.Errorf("expected at least 3 backend requests, got %d", bm.RequestsTotal)
		}
		if bm.BytesSent < 15 { // 3 * 5 bytes
			t.Errorf("expected at least 15 bytes sent, got %d", bm.BytesSent)
		}
	} else {
		t.Errorf("expected backend metrics for %s", backendAddr)
	}
}

// TestTCPProxyWeightedRoundRobin verifies weighted load balancing.
func TestTCPProxyWeightedRoundRobin(t *testing.T) {
	backend1 := startStaticServer(t, "heavy")
	defer backend1.Close()
	backend2 := startStaticServer(t, "light")
	defer backend2.Close()

	// Weight 2:1
	backends := []*backend.Backend{
		backend.NewBackendWithWeight(backend1.Addr().String(), 2),
		backend.NewBackendWithWeight(backend2.Addr().String(), 1),
	}
	lb := loadbalancer.NewWeightedRoundRobin(backends)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	proxy := NewTCPProxy(":0", lb, logger)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := proxy.Start(ctx); err != nil {
		t.Fatalf("failed to start proxy: %v", err)
	}
	<-proxy.Ready()
	defer proxy.Stop(context.Background())

	// Make 30 connections (10 full cycles of weight sum 3)
	responses := make(map[string]int)
	for i := 0; i < 30; i++ {
		client, err := net.Dial("tcp", proxy.Addr())
		if err != nil {
			t.Fatalf("dial failed: %v", err)
		}
		client.SetDeadline(time.Now().Add(1 * time.Second))
		client.Write([]byte("ping"))
		buf := make([]byte, 16)
		n, _ := client.Read(buf)
		client.Close()
		responses[string(buf[:n])]++
	}

	// Heavy should get ~20, light should get ~10
	heavy := responses["heavy"]
	light := responses["light"]

	if heavy < 18 || heavy > 22 {
		t.Errorf("expected ~20 heavy requests, got %d", heavy)
	}
	if light < 8 || light > 12 {
		t.Errorf("expected ~10 light requests, got %d", light)
	}
}

// Helper: startEchoServer creates a TCP server that echoes back whatever it receives.
func startEchoServer(t *testing.T) net.Listener {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to start echo server: %v", err)
	}

	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return // Listener closed
			}

			go func(c net.Conn) {
				defer c.Close()
				c.SetDeadline(time.Now().Add(5 * time.Second))
				buf := make([]byte, 4096)
				for {
					n, err := c.Read(buf)
					if err != nil {
						return
					}
					if _, err := c.Write(buf[:n]); err != nil {
						return
					}
				}
			}(conn)
		}
	}()

	return listener
}

// startStaticServer writes a fixed response for each connection.
func startStaticServer(t *testing.T, resp string) net.Listener {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to start static server: %v", err)
	}

	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				buf := make([]byte, 256)
				c.Read(buf)
				c.Write([]byte(resp))
			}(conn)
		}
	}()

	return listener
}

// buildRoundRobin constructs a simple round-robin load balancer from listeners.
func buildRoundRobin(listeners ...net.Listener) *loadbalancer.RoundRobin {
	backends := make([]*backend.Backend, len(listeners))
	for i, l := range listeners {
		backends[i] = backend.NewBackend(l.Addr().String())
	}
	return loadbalancer.NewRoundRobin(backends)
}

// buildRoundRobinWithPooling constructs a round-robin LB and enables pooling on backends when requested.
func buildRoundRobinWithPooling(enablePool bool, listeners ...net.Listener) *loadbalancer.RoundRobin {
	backends := make([]*backend.Backend, len(listeners))
	for i, l := range listeners {
		b := backend.NewBackend(l.Addr().String())
		if enablePool {
			cfg := backend.DefaultPoolConfig()
			cfg.MaxIdle = 256
			b.EnablePooling(&cfg)
		}
		backends[i] = b
	}
	return loadbalancer.NewRoundRobin(backends)
}

// Helper: startBidirectionalServer creates a server that reads lines and echoes with prefix.
func startBidirectionalServer(t *testing.T) net.Listener {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to start bidirectional server: %v", err)
	}

	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}

			go func(c net.Conn) {
				defer c.Close()
				buf := make([]byte, 1024)
				for {
					n, err := c.Read(buf)
					if err != nil {
						return
					}
					response := "echo: " + string(buf[:n])
					if _, err := c.Write([]byte(response)); err != nil {
						return
					}
				}
			}(conn)
		}
	}()

	return listener
}

// Helper: startSlowEchoServer adds artificial delay before echoing.
func startSlowEchoServer(t *testing.T, delay time.Duration) net.Listener {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to start slow echo server: %v", err)
	}

	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}

			go func(c net.Conn) {
				defer c.Close()
				buf := make([]byte, 1024)
				n, err := c.Read(buf)
				if err != nil {
					return
				}
				time.Sleep(delay) // Artificial delay
				c.Write(buf[:n])
			}(conn)
		}
	}()

	return listener
}

// Benchmark: BenchmarkTCPProxyThroughput measures proxy throughput.
func BenchmarkTCPProxyThroughput(b *testing.B) {
	bListener := mustStartEchoServer()
	defer bListener.Close()

	lb := buildRoundRobin(bListener)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	proxy := NewTCPProxy(":0", lb, logger)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Start proxy (non-blocking)
	if err := proxy.Start(ctx); err != nil {
		b.Fatalf("failed to start proxy: %v", err)
	}
	<-proxy.Ready()
	defer proxy.Stop(context.Background())

	client, err := net.Dial("tcp", proxy.Addr())
	if err != nil {
		b.Fatalf("failed to connect: %v", err)
	}
	defer client.Close()

	msg := []byte("benchmark")
	buf := make([]byte, len(msg))

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := client.Write(msg); err != nil {
			b.Fatalf("write failed: %v", err)
		}
		if _, err := io.ReadFull(client, buf); err != nil {
			b.Fatalf("read failed: %v", err)
		}
	}
}

// Benchmark: compare throughput with backend TCP pooling enabled.
func BenchmarkTCPProxyThroughputWithTCPPool(b *testing.B) {
	bListener := mustStartEchoServer()
	defer bListener.Close()

	lb := buildRoundRobinWithPooling(true, bListener)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	proxy := NewTCPProxy(":0", lb, logger)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := proxy.Start(ctx); err != nil {
		b.Fatalf("failed to start proxy: %v", err)
	}
	<-proxy.Ready()
	defer proxy.Stop(context.Background())

	client, err := net.Dial("tcp", proxy.Addr())
	if err != nil {
		b.Fatalf("failed to connect: %v", err)
	}
	defer client.Close()

	msg := []byte("benchmark")
	buf := make([]byte, len(msg))

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := client.Write(msg); err != nil {
			b.Fatalf("write failed: %v", err)
		}
		if _, err := io.ReadFull(client, buf); err != nil {
			b.Fatalf("read failed: %v", err)
		}
	}
}

// Benchmark: BenchmarkTCPProxyConcurrency measures concurrent client handling.
func BenchmarkTCPProxyConcurrency(b *testing.B) {
	bListener := mustStartEchoServer()
	defer bListener.Close()

	lb := buildRoundRobin(bListener)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	proxy := NewTCPProxy(":0", lb, logger)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Start proxy (non-blocking)
	if err := proxy.Start(ctx); err != nil {
		b.Fatalf("failed to start proxy: %v", err)
	}
	<-proxy.Ready()
	defer proxy.Stop(context.Background())

	b.RunParallel(func(pb *testing.PB) {
		client, err := net.Dial("tcp", proxy.Addr())
		if err != nil {
			b.Fatalf("failed to connect: %v", err)
		}
		defer client.Close()

		msg := []byte("test")
		buf := make([]byte, len(msg))

		for pb.Next() {
			client.Write(msg)
			io.ReadFull(client, buf)
		}
	})
}

// Benchmark: high-parallel load with backend pooling enabled (approx soak).
func BenchmarkTCPProxyConcurrencyWithTCPPool(b *testing.B) {
	bListener := mustStartEchoServer()
	defer bListener.Close()

	lb := buildRoundRobinWithPooling(true, bListener)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	proxy := NewTCPProxy(":0", lb, logger)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := proxy.Start(ctx); err != nil {
		b.Fatalf("failed to start proxy: %v", err)
	}
	<-proxy.Ready()
	defer proxy.Stop(context.Background())

	b.SetParallelism(16) // encourage many simultaneous clients
	b.ReportAllocs()

	b.RunParallel(func(pb *testing.PB) {
		client, err := net.Dial("tcp", proxy.Addr())
		if err != nil {
			b.Fatalf("failed to connect: %v", err)
		}
		defer client.Close()

		msg := []byte("test")
		buf := make([]byte, len(msg))

		for pb.Next() {
			client.Write(msg)
			io.ReadFull(client, buf)
		}
	})
}

func mustStartEchoServer() net.Listener {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		panic(err)
	}

	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				c.SetDeadline(time.Now().Add(30 * time.Second))
				buf := make([]byte, 4096)
				for {
					n, err := c.Read(buf)
					if err != nil {
						return
					}
					if _, err := c.Write(buf[:n]); err != nil {
						return
					}
				}
			}(conn)
		}
	}()

	return listener
}

// Benchmarks: quantify buffer pooling improvements for copy operations.
// These benchmarks isolate the copy path used by the TCP proxy to show
// allocations/op differences when using sync.Pool-backed buffers vs fresh
// allocations each call.

var benchCopyBufPool = sync.Pool{New: func() any { return make([]byte, 32*1024) }}

func BenchmarkCopyBufferWithPool(b *testing.B) {
	benchmarkCopyBuffer(b, true)
}

func BenchmarkCopyBufferNoPool(b *testing.B) {
	benchmarkCopyBuffer(b, false)
}

func benchmarkCopyBuffer(b *testing.B, pooled bool) {
	payload := bytes.Repeat([]byte("x"), 64*1024) // 64KB payload simulating typical TCP chunking
	b.ReportAllocs()
	b.SetBytes(int64(len(payload)))

	for i := 0; i < b.N; i++ {
		reader := bytes.NewReader(payload)
		var buf []byte
		if pooled {
			buf = benchCopyBufPool.Get().([]byte)
		} else {
			buf = make([]byte, 32*1024)
		}

		if _, err := io.CopyBuffer(io.Discard, reader, buf); err != nil {
			b.Fatalf("copy failed: %v", err)
		}

		if pooled {
			// Reset slice length before returning to pool
			buf = buf[:cap(buf)]
			benchCopyBufPool.Put(buf)
		}
	}
}
