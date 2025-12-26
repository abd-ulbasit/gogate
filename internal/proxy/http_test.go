package proxy

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
	"gogate/internal/circuitbreaker"
	"gogate/internal/loadbalancer"
)

// mockLoadBalancer returns backends in order for testing.
type mockLoadBalancer struct {
	backends []*backend.Backend
	index    int
}

func (m *mockLoadBalancer) Next() (*backend.Backend, error) {
	if len(m.backends) == 0 {
		return nil, loadbalancer.ErrNoBackends
	}
	b := m.backends[m.index%len(m.backends)]
	m.index++
	return b, nil
}

func (m *mockLoadBalancer) Backends() []*backend.Backend {
	return m.backends
}

func (m *mockLoadBalancer) AddBackend(b *backend.Backend) {
	m.backends = append(m.backends, b)
}

func (m *mockLoadBalancer) RemoveBackend(addr string) bool {
	for i, b := range m.backends {
		if b.Addr() == addr {
			m.backends = append(m.backends[:i], m.backends[i+1:]...)
			return true
		}
	}
	return false
}

func (m *mockLoadBalancer) UpdateBackends(backends []*backend.Backend) {
	m.backends = backends
}

func TestHTTPProxyBasicForwarding(t *testing.T) {
	// Create backend server
	backendServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Backend", "test")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("hello from backend"))
	}))
	defer backendServer.Close()

	// Create backend and load balancer
	b := backend.NewBackend(strings.TrimPrefix(backendServer.URL, "http://"))
	lb := &mockLoadBalancer{backends: []*backend.Backend{b}}

	// Create proxy
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	proxy := NewHTTPProxy(lb, logger)

	// Test request through proxy
	req := httptest.NewRequest("GET", "/test", nil)
	rec := httptest.NewRecorder()

	proxy.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", rec.Code)
	}
	if rec.Body.String() != "hello from backend" {
		t.Errorf("expected 'hello from backend', got %s", rec.Body.String())
	}
	if rec.Header().Get("X-Backend") != "test" {
		t.Errorf("expected X-Backend header, got %v", rec.Header())
	}
}

func TestHTTPProxyPostWithBody(t *testing.T) {
	// Create backend that echoes the body
	backendServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" {
			t.Errorf("expected POST, got %s", r.Method)
		}
		body, _ := io.ReadAll(r.Body)
		w.Write(body)
	}))
	defer backendServer.Close()

	b := backend.NewBackend(strings.TrimPrefix(backendServer.URL, "http://"))
	lb := &mockLoadBalancer{backends: []*backend.Backend{b}}

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	proxy := NewHTTPProxy(lb, logger)

	const inputText = `{"name":"test"}`
	body := strings.NewReader(inputText)
	req := httptest.NewRequest("POST", "/api/users", body)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	proxy.ServeHTTP(rec, req)

	if rec.Body.String() != inputText {
		t.Errorf("expected body to be echoed, got %s", rec.Body.String())
	}
}

func TestHTTPProxyHeaderPropagation(t *testing.T) {
	// Create backend that checks headers
	backendServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Check custom header was forwarded
		if r.Header.Get("X-Custom") != "value" {
			t.Errorf("expected X-Custom header, got %s", r.Header.Get("X-Custom"))
		}
		// Hop-by-hop headers are handled by the proxy, not removed from request
		// But the proxy sets Close=true which means Connection: close
		w.WriteHeader(http.StatusOK)
	}))
	defer backendServer.Close()

	b := backend.NewBackend(strings.TrimPrefix(backendServer.URL, "http://"))
	lb := &mockLoadBalancer{backends: []*backend.Backend{b}}

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	proxy := NewHTTPProxy(lb, logger)

	req := httptest.NewRequest("GET", "/", nil)
	req.Header.Set("X-Custom", "value")
	req.Header.Set("Connection", "keep-alive")
	rec := httptest.NewRecorder()

	proxy.ServeHTTP(rec, req)
}

func TestHTTPProxyBackendError(t *testing.T) {
	// Create backend that returns 500
	backendServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte("internal error"))
	}))
	defer backendServer.Close()

	b := backend.NewBackend(strings.TrimPrefix(backendServer.URL, "http://"))
	lb := &mockLoadBalancer{backends: []*backend.Backend{b}}

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	proxy := NewHTTPProxy(lb, logger)

	req := httptest.NewRequest("GET", "/", nil)
	rec := httptest.NewRecorder()

	proxy.ServeHTTP(rec, req)

	// Proxy should forward the 500
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("expected 500, got %d", rec.Code)
	}
}

func TestHTTPProxyNoBackend(t *testing.T) {
	// Empty load balancer
	lb := &mockLoadBalancer{backends: nil}

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	proxy := NewHTTPProxy(lb, logger)

	req := httptest.NewRequest("GET", "/", nil)
	rec := httptest.NewRecorder()

	proxy.ServeHTTP(rec, req)

	// Should return 503 Service Unavailable
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("expected 503, got %d", rec.Code)
	}
}

func TestHTTPProxyUnreachableBackend(t *testing.T) {
	// Backend on port that's not listening
	b := backend.NewBackend("127.0.0.1:59999")
	lb := &mockLoadBalancer{backends: []*backend.Backend{b}}

	// Use short timeout transport
	transport := &http.Transport{
		DialContext: (&httpDialer{timeout: 100 * time.Millisecond}).DialContext,
	}

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	proxy := NewHTTPProxy(lb, logger, WithHTTPTransport(transport))

	req := httptest.NewRequest("GET", "/", nil)
	rec := httptest.NewRecorder()

	proxy.ServeHTTP(rec, req)

	// Should return 502 Bad Gateway
	if rec.Code != http.StatusBadGateway {
		t.Errorf("expected 502, got %d", rec.Code)
	}
}

// httpDialer is a helper for creating transports with custom timeouts.
type httpDialer struct {
	timeout time.Duration
}

func (d *httpDialer) DialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	dialer := &net.Dialer{Timeout: d.timeout}
	return dialer.DialContext(ctx, network, addr)
}

func TestHTTPProxyWithCircuitBreaker(t *testing.T) {
	// The circuit breaker tracks transport-level failures, not HTTP 5xx responses
	// So we need to test with actual connection failures
	// Use a backend that will be closed after some requests

	// Create backend on a port that will refuse connections after we close it
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := listener.Addr().String()

	// Accept one connection then close
	go func() {
		conn, _ := listener.Accept()
		if conn != nil {
			// Respond to one request then close
			conn.Write([]byte("HTTP/1.1 200 OK\r\nContent-Length: 2\r\n\r\nok"))
			conn.Close()
		}
		listener.Close()
	}()

	b := backend.NewBackend(addr)
	lb := &mockLoadBalancer{backends: []*backend.Backend{b}}

	// Configure circuit breaker with low threshold
	cbConfig := circuitbreaker.Config{
		FailureThreshold: 2,
		SuccessThreshold: 1,
		Timeout:          50 * time.Millisecond,
		Window:           1 * time.Second,
	}

	// Use short timeout transport
	transport := &http.Transport{
		DialContext: (&httpDialer{timeout: 50 * time.Millisecond}).DialContext,
	}

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	proxy := NewHTTPProxy(lb, logger, WithHTTPCircuitBreaker(cbConfig), WithHTTPTransport(transport))

	// First request succeeds
	req := httptest.NewRequest("GET", "/", nil)
	rec := httptest.NewRecorder()
	proxy.ServeHTTP(rec, req)

	// Wait for listener to fully close
	time.Sleep(20 * time.Millisecond)

	// Next requests will fail (connection refused)
	for i := 0; i < 3; i++ {
		req := httptest.NewRequest("GET", "/", nil)
		rec := httptest.NewRecorder()
		proxy.ServeHTTP(rec, req)
		// These should return 502 Bad Gateway
		if rec.Code != http.StatusBadGateway {
			t.Logf("request %d: got %d", i+1, rec.Code)
		}
	}

	// After enough failures, circuit should be open
	// Next request should be blocked by circuit breaker
	req = httptest.NewRequest("GET", "/", nil)
	rec = httptest.NewRecorder()
	proxy.ServeHTTP(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("expected 503 (circuit open), got %d", rec.Code)
	}
}

func TestHTTPProxyActiveRequests(t *testing.T) {
	// Create slow backend
	started := make(chan struct{})
	finish := make(chan struct{})
	backendServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(started)
		<-finish
		w.WriteHeader(http.StatusOK)
	}))
	defer backendServer.Close()

	b := backend.NewBackend(strings.TrimPrefix(backendServer.URL, "http://"))
	lb := &mockLoadBalancer{backends: []*backend.Backend{b}}

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	proxy := NewHTTPProxy(lb, logger)

	if proxy.ActiveRequests() != 0 {
		t.Errorf("expected 0 active requests, got %d", proxy.ActiveRequests())
	}

	// Start request in background
	done := make(chan struct{})
	go func() {
		req := httptest.NewRequest("GET", "/", nil)
		rec := httptest.NewRecorder()
		proxy.ServeHTTP(rec, req)
		close(done)
	}()

	// Wait for request to start
	<-started

	if proxy.ActiveRequests() != 1 {
		t.Errorf("expected 1 active request, got %d", proxy.ActiveRequests())
	}

	// Let request finish
	close(finish)
	<-done

	if proxy.ActiveRequests() != 0 {
		t.Errorf("expected 0 active requests after completion, got %d", proxy.ActiveRequests())
	}
}

func TestCopyHeaders(t *testing.T) {
	src := http.Header{}
	src.Set("Content-Type", "application/json")
	src.Set("X-Custom", "value")
	src.Set("Connection", "keep-alive") // Hop-by-hop, should be skipped

	dst := http.Header{}
	copyHeaders(dst, src)

	if dst.Get("Content-Type") != "application/json" {
		t.Error("Content-Type not copied")
	}
	if dst.Get("X-Custom") != "value" {
		t.Error("X-Custom not copied")
	}
	if dst.Get("Connection") != "" {
		t.Error("Connection header should not be copied")
	}
}

func TestRemoveHopByHopHeaders(t *testing.T) {
	header := http.Header{}
	header.Set("Content-Type", "application/json")
	header.Set("Connection", "keep-alive")
	header.Set("Keep-Alive", "timeout=5")
	header.Set("Transfer-Encoding", "chunked")

	removeHopByHopHeaders(header)

	if header.Get("Content-Type") != "application/json" {
		t.Error("Content-Type should not be removed")
	}
	if header.Get("Connection") != "" {
		t.Error("Connection should be removed")
	}
	if header.Get("Keep-Alive") != "" {
		t.Error("Keep-Alive should be removed")
	}
	if header.Get("Transfer-Encoding") != "" {
		t.Error("Transfer-Encoding should be removed")
	}
}

func TestDefaultTransport(t *testing.T) {
	transport := DefaultTransport()

	if transport.MaxIdleConns != 100 {
		t.Errorf("expected MaxIdleConns=100, got %d", transport.MaxIdleConns)
	}
	if transport.MaxIdleConnsPerHost != 10 {
		t.Errorf("expected MaxIdleConnsPerHost=10, got %d", transport.MaxIdleConnsPerHost)
	}
	if !transport.DisableCompression {
		t.Error("expected DisableCompression=true")
	}
}
