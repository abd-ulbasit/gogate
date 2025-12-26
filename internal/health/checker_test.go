package health

import (
	"context"
	"io"
	"log/slog"
	"net"
	"testing"
	"time"

	"gogate/internal/backend"
)

// startTestServer starts a simple TCP server that accepts connections.
func startTestServer(t *testing.T, addr string) net.Listener {
	t.Helper()
	l, err := net.Listen("tcp", addr)
	if err != nil {
		t.Fatalf("listen failed: %v", err)
	}
	go func() {
		for {
			conn, err := l.Accept()
			if err != nil {
				return // listener closed
			}
			go func(c net.Conn) {
				defer c.Close()
				buf := make([]byte, 4)
				c.SetDeadline(time.Now().Add(2 * time.Second))
				c.Read(buf)
				c.Write([]byte("ok"))
			}(conn)
		}
	}()
	return l
}

// waitFor polls condition until it returns true or timeout expires.
func waitFor(t *testing.T, timeout time.Duration, cond func() bool) {
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

func TestCheckerMarksUnhealthyAndRecovers(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	// Start healthy server
	listener := startTestServer(t, "127.0.0.1:0")
	addr := listener.Addr().String()
	backends := []*backend.Backend{backend.NewBackend(addr)}

	cfg := CheckerConfig{
		Interval:           50 * time.Millisecond,
		Timeout:            50 * time.Millisecond,
		UnhealthyThreshold: 1,
		HealthyThreshold:   1,
		CheckType:          "tcp",
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	checker := NewChecker(backends, logger, cfg)
	if err := checker.Start(ctx); err != nil {
		t.Fatalf("checker start: %v", err)
	}

	// Should become healthy
	waitFor(t, 500*time.Millisecond, func() bool { return backends[0].IsHealthy() })

	// Kill server -> should become unhealthy
	listener.Close()
	waitFor(t, 1*time.Second, func() bool { return !backends[0].IsHealthy() })

	// Restart server on same addr -> should recover
	listener = startTestServer(t, addr)
	defer listener.Close()
	waitFor(t, 1*time.Second, func() bool { return backends[0].IsHealthy() })

	checker.Stop()
}
