package backend

import (
	"context"
	"io"
	"net"
	"testing"
	"time"
)

// =============================================================================
func waitUntil(t *testing.T, timeout time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("condition not met within %s", timeout)
}

// Test Scaffolding for Backend Package
// =============================================================================
// These tests validate the Backend abstraction. Your job is to:
// 1. Fill in the test case values in the table-driven tests
// 2. Think about edge cases and add more test cases
// 3. Run with -race to catch concurrency bugs
// =============================================================================

func TestNewBackend(t *testing.T) {
	// TODO(basit): Add test cases for NewBackend
	tests := []struct {
		name    string
		addr    string
		wantErr bool
	}{
		{
			name:    "valid address",
			addr:    "localhost:8080",
			wantErr: false,
		},
		// TODO(basit): Add more test cases:
		// - empty address
		// - IPv6 address
		// - address with hostname
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			b := NewBackend(tt.addr)
			if b == nil {
				t.Fatal("NewBackend returned nil")
			}
			if b.Addr() != tt.addr {
				t.Errorf("Addr() = %q, want %q", b.Addr(), tt.addr)
			}
		})
	}
}

func TestBackendHealthState(t *testing.T) {
	b := NewBackend("localhost:8080")

	// Initial state should be healthy (optimistic)
	if !b.IsHealthy() {
		t.Error("new backend should be healthy by default")
	}

	// Test SetHealthy
	b.SetHealthy(false, nil)
	if b.IsHealthy() {
		t.Error("backend should be unhealthy after SetHealthy(false, nil)")
	}

	b.SetHealthy(true, nil)
	if !b.IsHealthy() {
		t.Error("backend should be healthy after SetHealthy(true, nil)")
	}
}

func TestBackendActiveConnections(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer listener.Close()

	// simple server accepting and keeping open until test end
	done := make(chan struct{})
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				select {
				case <-done:
					c.Close()
				case <-time.After(2 * time.Second):
					c.Close()
				}
			}(conn)
		}
	}()

	b := NewBackend(listener.Addr().String())
	ctx := context.Background()

	conn1, err := b.Dial(ctx)
	if err != nil {
		t.Fatalf("dial1: %v", err)
	}

	conn2, err := b.Dial(ctx)
	if err != nil {
		t.Fatalf("dial2: %v", err)
	}

	if got := b.ActiveConnections(); got != 2 {
		t.Fatalf("active connections = %d, want 2", got)
	}

	conn1.Close()
	conn2.Close()
	close(done)

	waitUntil(t, 500*time.Millisecond, func() bool { return b.ActiveConnections() == 0 })
}

func TestBackendStats(t *testing.T) {
	// Stats are tracked through Dial() calls
	// TotalConnections and Failures are incremented internally
	b := NewBackend("localhost:8080")

	// Initial state
	if b.TotalConnections() != 0 {
		t.Errorf("initial TotalConnections() = %d, want 0", b.TotalConnections())
	}
	if b.Failures() != 0 {
		t.Errorf("initial Failures() = %d, want 0", b.Failures())
	}
}

// TestBackendDialSuccess tests dialing to a real server
func TestBackendDialSuccess(t *testing.T) {
	// Start a test server
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to create listener: %v", err)
	}
	defer listener.Close()

	// Accept connections in background
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			conn.Close()
		}
	}()

	b := NewBackend(listener.Addr().String())

	// Dial should succeed
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	conn, err := b.Dial(ctx)
	if err != nil {
		t.Fatalf("Dial() failed: %v", err)
	}
	conn.Close()

	// Should have recorded the connection
	if b.TotalConnections() != 1 {
		t.Errorf("TotalConnections = %d, want 1", b.TotalConnections())
	}
	if b.Failures() != 0 {
		t.Errorf("Failures = %d, want 0", b.Failures())
	}
}

// TestBackendDialFailure tests dialing to a non-existent server
func TestBackendDialFailure(t *testing.T) {
	// Use a port that's very unlikely to have anything listening
	b := NewBackend("127.0.0.1:59999")

	// Dial should fail
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	conn, err := b.Dial(ctx)
	if err == nil {
		conn.Close()
		t.Fatal("Dial() should have failed")
	}

	// Should have recorded failure
	if b.TotalConnections() != 1 {
		t.Errorf("TotalConnections = %d, want 1", b.TotalConnections())
	}
	if b.Failures() != 1 {
		t.Errorf("Failures = %d, want 1", b.Failures())
	}
}

// Benchmarks: pooled vs non-pooled dials

func BenchmarkBackendDialNoPool(b *testing.B) {
	benchmarkBackendDial(b, false)
}

func BenchmarkBackendDialWithPool(b *testing.B) {
	benchmarkBackendDial(b, true)
}

func benchmarkBackendDial(b *testing.B, pooled bool) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		b.Fatalf("listen: %v", err)
	}
	defer listener.Close()

	// Echo server keeps connections open and echoes any bytes.
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				buf := make([]byte, 32)
				for {
					c.SetDeadline(time.Now().Add(2 * time.Second))
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

	be := NewBackend(listener.Addr().String())
	if pooled {
		cfg := DefaultPoolConfig()
		cfg.MaxIdle = 128
		be.EnablePooling(&cfg)
	} else {
		be.EnablePooling(nil)
	}

	ctx := context.Background()
	payload := []byte("x")
	buf := make([]byte, len(payload))

	b.ReportAllocs()
	b.SetBytes(int64(len(payload)))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		conn, err := be.Dial(ctx)
		if err != nil {
			b.Fatalf("dial: %v", err)
		}
		conn.SetDeadline(time.Now().Add(1 * time.Second))
		if _, err := conn.Write(payload); err != nil {
			conn.Close()
			b.Fatalf("write: %v", err)
		}
		if _, err := io.ReadFull(conn, buf); err != nil {
			conn.Close()
			b.Fatalf("read: %v", err)
		}
		conn.Close()

		// Throttle to avoid exhausting ephemeral ports in no-pool mode during benchmarks
		time.Sleep(200 * time.Microsecond)
	}
}
