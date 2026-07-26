package health

import (
	"context"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
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

// =============================================================================
// HTTP health checks
// =============================================================================

// TestHTTPCheckDistinguishesServingFromListening is the reason check_type: http
// exists. A TCP check only proves the kernel accepted a connection, which it
// does on behalf of a process that has stopped serving. This asserts the HTTP
// check separates the two cases that a TCP check collapses into "healthy".
func TestHTTPCheckDistinguishesServingFromListening(t *testing.T) {
	tests := []struct {
		name        string
		handler     http.HandlerFunc
		path        string
		wantHealthy bool
	}{
		{
			name:        "200 is healthy",
			handler:     func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) },
			wantHealthy: true,
		},
		{
			name:        "204 is healthy",
			handler:     func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusNoContent) },
			wantHealthy: true,
		},
		{
			name: "503 is unhealthy: the backend is telling us it is not ready",
			handler: func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusServiceUnavailable)
			},
			wantHealthy: false,
		},
		{
			name: "404 is unhealthy: the health endpoint is not where we were told",
			handler: func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusNotFound)
			},
			wantHealthy: false,
		},
		{
			name: "500 is unhealthy",
			handler: func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusInternalServerError)
			},
			wantHealthy: false,
		},
		{
			name: "redirect is unhealthy rather than followed",
			handler: func(w http.ResponseWriter, r *http.Request) {
				http.Redirect(w, r, "http://example.invalid/health", http.StatusFound)
			},
			wantHealthy: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := httptest.NewServer(tt.handler)
			defer srv.Close()

			addr := strings.TrimPrefix(srv.URL, "http://")
			c := NewChecker(nil, slog.New(slog.NewTextHandler(io.Discard, nil)), CheckerConfig{
				CheckType: "http",
				HTTPPath:  "/health",
				Timeout:   2 * time.Second,
			})

			err := c.httpCheck(context.Background(), addr)
			if tt.wantHealthy && err != nil {
				t.Errorf("httpCheck = %v, want healthy", err)
			}
			if !tt.wantHealthy && err == nil {
				t.Error("httpCheck reported healthy, want an error")
			}
		})
	}
}

// TestHTTPCheckFailsOnListeningButNotServing covers the case a TCP check cannot
// see: a socket that accepts connections and then never speaks HTTP.
func TestHTTPCheckFailsOnListeningButNotServing(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	// Accept and hold, never respond.
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			defer conn.Close()
		}
	}()

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	c := NewChecker(nil, logger, CheckerConfig{
		CheckType: "http",
		HTTPPath:  "/health",
		Timeout:   500 * time.Millisecond,
	})

	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()

	if err := c.httpCheck(ctx, ln.Addr().String()); err == nil {
		t.Error("httpCheck reported healthy for a socket that accepts but never serves")
	}

	// The same backend passes a TCP check, which is the whole point.
	tcpCtx, tcpCancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer tcpCancel()
	if err := c.tcpCheck(tcpCtx, ln.Addr().String()); err != nil {
		t.Errorf("tcpCheck = %v, want success (it only proves the port is open)", err)
	}
}

// TestHTTPCheckNormalizesPath covers a path configured without a leading slash.
func TestHTTPCheckNormalizesPath(t *testing.T) {
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	c := NewChecker(nil, slog.New(slog.NewTextHandler(io.Discard, nil)), CheckerConfig{
		CheckType: "http",
		HTTPPath:  "healthz",
		Timeout:   2 * time.Second,
	})
	if err := c.httpCheck(context.Background(), strings.TrimPrefix(srv.URL, "http://")); err != nil {
		t.Fatalf("httpCheck: %v", err)
	}
	if gotPath != "/healthz" {
		t.Errorf("backend saw path %q, want %q", gotPath, "/healthz")
	}
}

// TestHTTPCheckReusesOneClient guards the shared-client design: a fresh
// http.Client per probe would drop the connection pool every interval.
func TestHTTPCheckReusesOneClient(t *testing.T) {
	c := NewChecker(nil, slog.New(slog.NewTextHandler(io.Discard, nil)), DefaultCheckerConfig())
	first := c.httpClient()
	second := c.httpClient()
	if first != second {
		t.Error("httpClient() returned a different client on the second call")
	}
	if first.Transport == nil {
		t.Error("httpClient() has no transport; every probe would use DefaultTransport's pool")
	}
}
