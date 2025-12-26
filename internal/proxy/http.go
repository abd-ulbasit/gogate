package proxy

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"gogate/internal/backend"
	"gogate/internal/circuitbreaker"
	"gogate/internal/loadbalancer"
	"gogate/internal/metrics"
)

// HTTPProxy is a Layer 7 (HTTP) reverse proxy.
//
// Design:
// - Reuses existing LoadBalancer, CircuitBreaker, Metrics from L4 proxy
// - Uses standard net/http for serving
// - Implements http.Handler for composability with middleware
//
// Key responsibilities:
// 1. Select backend via load balancer
// 2. Check circuit breaker
// 3. Clone request and forward to backend
// 4. Copy response back to client
// 5. Record metrics
//
// Why not just use httputil.ReverseProxy?
// - Learning: Understand reverse proxy internals
// - Control: Integrate with our circuit breaker, load balancer, metrics
// - Features: Custom retry logic, request modification
//
// Thread safety: All methods are safe for concurrent use.
type HTTPProxy struct {
	lb        loadbalancer.LoadBalancer
	logger    *slog.Logger
	transport http.RoundTripper

	// Optional features (same pattern as TCPProxy)
	circuitBreakers  map[string]*circuitbreaker.CircuitBreaker
	cbMu             sync.RWMutex
	cbConfig         circuitbreaker.Config
	metricsCollector *metrics.Collector

	// Stats
	activeRequests atomic.Int64
}

// HTTPOption configures optional HTTPProxy features.
type HTTPOption func(*HTTPProxy)

// WithHTTPCircuitBreaker enables per-backend circuit breakers.
func WithHTTPCircuitBreaker(config circuitbreaker.Config) HTTPOption {
	return func(p *HTTPProxy) {
		p.circuitBreakers = make(map[string]*circuitbreaker.CircuitBreaker)
		p.cbConfig = config
	}
}

// WithHTTPMetrics enables metrics collection.
func WithHTTPMetrics(collector *metrics.Collector) HTTPOption {
	return func(p *HTTPProxy) {
		p.metricsCollector = collector
	}
}

// WithHTTPTransport sets a custom http.RoundTripper.
//
// Use this to configure connection pooling, timeouts, TLS, etc.
// If not set, http.DefaultTransport is used.
func WithHTTPTransport(transport http.RoundTripper) HTTPOption {
	return func(p *HTTPProxy) {
		p.transport = transport
	}
}

// WithHTTPTransportConfig builds a transport from the provided config and sets it.
func WithHTTPTransportConfig(cfg HTTPTransportConfig) HTTPOption {
	return func(p *HTTPProxy) {
		p.transport = NewHTTPTransport(cfg)
	}
}

// NewHTTPProxy creates a new HTTP reverse proxy.
//
// Example:
//
//	proxy := NewHTTPProxy(lb, logger,
//	    WithHTTPCircuitBreaker(circuitbreaker.DefaultConfig()),
//	    WithHTTPMetrics(metrics.NewCollector()),
//	)
func NewHTTPProxy(lb loadbalancer.LoadBalancer, logger *slog.Logger, opts ...HTTPOption) *HTTPProxy {
	p := &HTTPProxy{
		lb:     lb,
		logger: logger,
		// Use tuned default transport with connection pooling
		transport: DefaultTransport(),
	}

	for _, opt := range opts {
		opt(p)
	}

	return p
}

// getCircuitBreaker returns the circuit breaker for a backend, creating one if needed.
func (p *HTTPProxy) getCircuitBreaker(addr string) *circuitbreaker.CircuitBreaker {
	if p.circuitBreakers == nil {
		return nil
	}

	p.cbMu.RLock()
	cb, ok := p.circuitBreakers[addr]
	p.cbMu.RUnlock()
	if ok {
		return cb
	}

	// Create new circuit breaker (double-checked locking)
	p.cbMu.Lock()
	defer p.cbMu.Unlock()
	if cb, ok := p.circuitBreakers[addr]; ok {
		return cb
	}
	cb = circuitbreaker.New(p.cbConfig)
	p.circuitBreakers[addr] = cb
	return cb
}

// ServeHTTP implements http.Handler.
//
// This is the core proxy logic:
// 1. Select backend from load balancer
// 2. Check circuit breaker
// 3. Clone and modify request
// 4. Forward to backend
// 5. Copy response
// 6. Record metrics
func (p *HTTPProxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	startTime := time.Now()
	p.activeRequests.Add(1)
	defer p.activeRequests.Add(-1)

	// Step 1: Select backend
	b, err := p.lb.Next()
	if err != nil {
		p.logger.Error("no backend available", "error", err)
		http.Error(w, "Service Unavailable", http.StatusServiceUnavailable)
		return
	}

	// Step 2: Check circuit breaker
	cb := p.getCircuitBreaker(b.Addr())
	if cb != nil {
		if err := cb.Allow(); err != nil {
			p.logger.Warn("circuit breaker open", "backend", b.Addr())
			http.Error(w, "Service Unavailable (circuit open)", http.StatusServiceUnavailable)
			return
		}
	}

	// Step 3: Clone and modify request for backend
	outReq := p.cloneRequest(r, b)

	// Step 4: Forward request to backend
	resp, err := p.transport.RoundTrip(outReq)
	if err != nil {
		p.logger.Error("backend request failed",
			"backend", b.Addr(),
			"error", err,
		)

		// Record failure in circuit breaker
		if cb != nil {
			cb.RecordResult(err)
		}

		// Classify error for response
		if errors.Is(err, context.DeadlineExceeded) {
			http.Error(w, "Gateway Timeout", http.StatusGatewayTimeout)
		} else {
			http.Error(w, "Bad Gateway", http.StatusBadGateway)
		}
		return
	}
	defer resp.Body.Close()

	// Record success in circuit breaker
	if cb != nil {
		cb.RecordResult(nil)
	}

	// Step 5: Copy response headers
	copyHeaders(w.Header(), resp.Header)
	w.WriteHeader(resp.StatusCode)

	// Step 6: Copy response body
	bytesCopied, _ := io.Copy(w, resp.Body)

	// Step 7: Record metrics
	latency := time.Since(startTime)
	if p.metricsCollector != nil {
		success := resp.StatusCode < 500
		p.metricsCollector.RecordRequest(b.Addr(), success, latency, 0, bytesCopied)
	}

	p.logger.Debug("http proxy request",
		"method", r.Method,
		"path", r.URL.Path,
		"backend", b.Addr(),
		"status", resp.StatusCode,
		"duration_ms", float64(latency.Microseconds())/1000,
		"bytes", bytesCopied,
	)
}

// cloneRequest creates a copy of the request for the backend.
//
// Why clone instead of modifying original?
// - Original request may be used by middleware after proxy
// - Concurrent access issues if we modify shared request
//
// Modifications made:
// - Host set to backend address
// - URL scheme and host updated
// - Hop-by-hop headers removed
// - RequestURI cleared (required by http.RoundTripper)
func (p *HTTPProxy) cloneRequest(r *http.Request, b *backend.Backend) *http.Request {
	// Create shallow copy of request
	outReq := r.Clone(r.Context())

	// Update URL to point to backend
	outReq.URL = &url.URL{
		Scheme:   "http", // TODO: Support HTTPS backends
		Host:     b.Addr(),
		Path:     r.URL.Path,
		RawQuery: r.URL.RawQuery,
	}

	// Set Host header to backend (some backends check this)
	outReq.Host = b.Addr()

	// Clear RequestURI (required by http.RoundTripper spec)
	outReq.RequestURI = ""

	// Remove hop-by-hop headers (not forwarded by proxies)
	removeHopByHopHeaders(outReq.Header)

	// Enable HTTP connection pooling (keep-alive). The configured Transport
	// will manage idle connections and pooling behavior.
	outReq.Close = false

	return outReq
}

// copyHeaders copies headers from src to dst.
//
// Skips hop-by-hop headers that shouldn't be forwarded.
func copyHeaders(dst, src http.Header) {
	for k, vv := range src {
		// Skip hop-by-hop headers
		if isHopByHopHeader(k) {
			continue
		}
		for _, v := range vv {
			dst.Add(k, v)
		}
	}
}

// Hop-by-hop headers are connection-specific and shouldn't be forwarded.
// https://developer.mozilla.org/en-US/docs/Web/HTTP/Headers#hop-by-hop_headers
var hopByHopHeaders = []string{
	"Connection",
	"Keep-Alive",
	"Proxy-Authenticate",
	"Proxy-Authorization",
	"Te",
	"Trailers",
	"Transfer-Encoding",
	"Upgrade",
}

func isHopByHopHeader(header string) bool {
	for _, h := range hopByHopHeaders {
		if strings.EqualFold(header, h) {
			return true
		}
	}
	return false
}

func removeHopByHopHeaders(header http.Header) {
	for _, h := range hopByHopHeaders {
		header.Del(h)
	}
}

// ActiveRequests returns the number of requests currently being processed.
func (p *HTTPProxy) ActiveRequests() int64 {
	return p.activeRequests.Load()
}

// DefaultTransport returns a pre-configured http.Transport suitable for proxying.
//
// Configuration:
// - MaxIdleConns: 100 (global connection pool)
// - MaxIdleConnsPerHost: 10 (per-backend connection pool)
// - IdleConnTimeout: 90s (cleanup idle connections)
// - DisableCompression: true (let backend handle compression)
//
// Use with WithHTTPTransport() for production deployments.
func DefaultTransport() *http.Transport {
	return NewHTTPTransport(DefaultHTTPTransportConfig())
}

// HTTPTransportConfig controls HTTP connection pooling and timeouts.
type HTTPTransportConfig struct {
	MaxIdleConns          int
	MaxIdleConnsPerHost   int
	MaxConnsPerHost       int
	IdleConnTimeout       time.Duration
	TLSHandshakeTimeout   time.Duration
	ExpectContinueTimeout time.Duration
	DialTimeout           time.Duration
	DialKeepAlive         time.Duration
	DisableCompression    bool
}

// DefaultHTTPTransportConfig provides sensible production defaults.
func DefaultHTTPTransportConfig() HTTPTransportConfig {
	return HTTPTransportConfig{
		MaxIdleConns:          100,
		MaxIdleConnsPerHost:   10,
		MaxConnsPerHost:       0,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
		DialTimeout:           30 * time.Second,
		DialKeepAlive:         30 * time.Second,
		DisableCompression:    true,
	}
}

// NewHTTPTransport builds an http.Transport from the given config.
func NewHTTPTransport(cfg HTTPTransportConfig) *http.Transport {
	if cfg.MaxIdleConns == 0 {
		cfg.MaxIdleConns = 100
	}
	if cfg.MaxIdleConnsPerHost == 0 {
		cfg.MaxIdleConnsPerHost = 10
	}
	if cfg.IdleConnTimeout == 0 {
		cfg.IdleConnTimeout = 90 * time.Second
	}
	if cfg.TLSHandshakeTimeout == 0 {
		cfg.TLSHandshakeTimeout = 10 * time.Second
	}
	if cfg.ExpectContinueTimeout == 0 {
		cfg.ExpectContinueTimeout = 1 * time.Second
	}
	if cfg.DialTimeout == 0 {
		cfg.DialTimeout = 30 * time.Second
	}
	if cfg.DialKeepAlive == 0 {
		cfg.DialKeepAlive = 30 * time.Second
	}

	return &http.Transport{
		Proxy: http.ProxyFromEnvironment,
		DialContext: (&net.Dialer{
			Timeout:   cfg.DialTimeout,
			KeepAlive: cfg.DialKeepAlive,
		}).DialContext,
		MaxIdleConns:          cfg.MaxIdleConns,
		MaxIdleConnsPerHost:   cfg.MaxIdleConnsPerHost,
		MaxConnsPerHost:       cfg.MaxConnsPerHost,
		IdleConnTimeout:       cfg.IdleConnTimeout,
		TLSHandshakeTimeout:   cfg.TLSHandshakeTimeout,
		ExpectContinueTimeout: cfg.ExpectContinueTimeout,
		DisableCompression:    cfg.DisableCompression,
	}
}
