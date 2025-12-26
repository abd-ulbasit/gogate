# GoGate Development Notes

## Status: Phase 1 Complete ✅

**Completed Milestones**: 10/10
**Tests**: 200+ passing (race-free)
**Coverage**: Full L4/L7 proxy with production features

### What's Included in Phase 1:
- ✅ TCP (L4) proxy with half-close semantics
- ✅ HTTP (L7) reverse proxy with middleware chain
- ✅ Load balancing (Round Robin, Weighted RR, Least Connections)
- ✅ Health checking (TCP/HTTP probes)
- ✅ Circuit breaker (three-state pattern)
- ✅ Rate limiting (token bucket)
- ✅ Traffic splitting (canary/A-B testing)
- ✅ JWT authentication
- ✅ Service discovery with TTL
- ✅ Hot reload via fsnotify
- ✅ Prometheus metrics endpoint
- ✅ Connection pooling
- ✅ Grafana dashboard
- ✅ Load testing suite (k6)

### Future Phase 2 Ideas (Not Started):
- Distributed rate limiting (Redis)
- TLS termination
- gRPC support
- WebSocket proxying
- Admin UI
- OpenTelemetry tracing

---

## Milestone History

### Session 1 Complete ✓ (Milestone 1)
- TCP proxy code reviewed and bugs fixed
- Comprehensive test suite written (7 tests + 2 benchmarks)
- Multi-client support confirmed via goroutine-based accept loop
- Understanding of bidirectional copy, half-close, graceful shutdown

### Session 2 Complete ✓ (Milestone 1)
- **Major Refactor**: Blocking → Non-blocking Start() pattern
- Code review and race condition fixes
- Tests updated to use new API (no more `go func()` + `time.Sleep` hacks)
- All 7 tests pass with race detector
- Benchmarks: ~51μs/op throughput, ~32μs/op concurrent

### Session 3 Complete ✓ (Milestone 2)
- Backend abstraction with health state, connection tracking
- LoadBalancer interface with RoundRobin and LeastConnections
- Active health checker with threshold-based transitions
- trackedConn wrapper for automatic connection counting
- Fixed half-close with CloseWrite interface pattern
- 24 tests passing across all packages

### Session 4 Complete ✓ (Milestone 3)
- Circuit breaker with state machine (CLOSED → OPEN → HALF-OPEN)
- Token bucket rate limiter with refill logic
- Metrics collector with latency histogram
- 22 new tests added (8 circuit breaker, 8 rate limiter, 6 metrics)
- Total: 46 tests across 7 packages, all passing

### Session 5 Complete ✓ (Milestone 4)
- **Weighted Round-Robin**: NGINX smooth weighted round-robin algorithm
- **Integration**: Circuit breaker, rate limiter, metrics integrated into proxy
- **Functional Options**: Clean API for optional features (`WithRateLimiter`, `WithCircuitBreaker`, `WithMetrics`)
- **Admin Server**: HTTP endpoints for `/stats`, `/backends`, `/health`
- **Config Updates**: Added rate_limit, circuit_breaker, admin sections
- 4 new integration tests + 7 weighted RR tests
- Total: 57 tests across 7 packages, all passing

### Session 6 Complete ✓ (Milestone 5)
- **HTTP Router**: Path prefix and host-based route matching with specificity scoring
- **Middleware Chain**: Standard Go middleware pattern with proper execution order
- **Logging Middleware**: Structured request/response logging with responseWriter wrapper
- **Recovery Middleware**: Panic recovery with stack trace logging
- **Rate Limit Middleware**: HTTP wrapper around existing TokenBucket
- **Headers Middleware**: X-Forwarded-For, X-Forwarded-Host, X-Forwarded-Proto, X-Real-IP
- **Request ID Middleware**: Generate/preserve X-Request-ID for correlation
- **Strip Prefix Middleware**: Path rewriting for proxied requests
- **HTTP Proxy**: Layer 7 reverse proxy with LoadBalancer, CircuitBreaker, Metrics integration
- **Context Helpers**: Route info passed via context for logging/middleware
- 25 new tests added (router: 4, middleware: 9, http proxy: 12)
- Total: 82 tests across 9 packages, all passing with race detector

### Session 7 Complete ✓ (Milestone 6)
- **Traffic Splitting**: Weighted traffic distribution for canary deployments
  - `Splitter`: Weighted random selection for statistical distribution
  - `SimpleSplitter`: Deterministic round-robin for exact percentages
  - Support for canary (95/5), A/B testing (50/50), multi-pool (60/30/10)
- **JWT Authentication**: Pure Go implementation (no external dependencies)
  - HS256 (HMAC-SHA256) token signing and verification
  - Claims validation (exp, iss, aud) with clock skew tolerance
  - Custom claims extraction for app-specific data
- **Auth Middleware**: JWT validation in HTTP middleware chain
  - Bearer token extraction from Authorization header
  - Claims stored in context for downstream handlers
  - SkipAuth for public endpoints (/health, /metrics)
  - OptionalAuth for mixed authenticated/anonymous access
- **CORS Middleware**: Cross-Origin Resource Sharing support
  - Preflight (OPTIONS) request handling
  - Configurable origins, methods, headers, credentials
  - Max-Age for browser caching of preflight results
- 32 new tests added (splitter: 9, JWT: 11, middleware: 12)
- Total: 114 tests across 11 packages, all passing with race detector

### Session 9 Complete ✓ (Milestone 8: Service Discovery & Hot Reload)
- **Service Registry**: In-memory registry with RWMutex
  - Service struct: ID, Name, Address, Metadata, Tags, TTL, LastHeartbeat
  - Register, Deregister, Get, List, Heartbeat operations
  - Concurrent access safe with proper locking
- **TTL Expiration**: Background cleanup goroutine
  - Ticker-based cleanup interval (configurable)
  - Automatic expiration when service misses heartbeat
  - Event notification on expiration
- **Watch Mechanism**: Channel-based pub/sub
  - Subscribe/Unsubscribe for registry events
  - Event types: Registered, Deregistered, Expired, Updated
  - Non-blocking sends (slow subscribers don't block)
- **HTTP API**: REST endpoints for registry operations
  - POST /v1/services - Register service
  - GET /v1/services - List all services
  - GET /v1/services/:id - Get single service
  - DELETE /v1/services/:id - Deregister service
  - PUT /v1/services/:id/heartbeat - Refresh TTL
- **Hot Reload (SIGHUP)**: Zero-downtime config reload
  - Signal handler in main.go
  - Re-reads config file, updates backends
  - LoadBalancer.UpdateBackends() for atomic replacement
  - No connection drops during reload
- **LoadBalancer Integration**: Added UpdateBackends() method
  - Interface method for dynamic backend updates
  - Implemented in RoundRobin, WeightedRoundRobin, LeastConnections
  - health.Checker.UpdateBackends() for health checking
- 57 new tests added (28+ registry tests, mock updates)
- Total: 171 tests across 13 packages, all passing with race detector

## What We've Built

### TCP Proxy (internal/proxy/tcp.go)
```go
// Non-blocking Start pattern with optional features
proxy := NewTCPProxy(":8080", lb, logger,
    WithRateLimiter(ratelimiter.NewTokenBucket(100, 10)),
    WithCircuitBreaker(circuitbreaker.DefaultConfig()),
    WithMetrics(metrics.NewCollector()),
)
proxy.Start(ctx)      // Returns immediately
<-proxy.Ready()       // Wait for accept loop to start
defer proxy.Stop(ctx) // Graceful shutdown

// Key methods:
Addr() string           // Get bound address (useful for :0)
Ready() <-chan struct{} // Signals when accepting
Done() <-chan struct{}  // Signals when stopped
ActiveConnections() int64
```

### Test Suite (internal/proxy/tcp_test.go)
- BasicForwarding, Bidirectional, ConcurrentConnections
- GracefulShutdown, BackendFailure, HalfClose, ActiveConnections
- LoadBalancerIntegration, WeightedRoundRobin
- WithRateLimiter, WithCircuitBreaker, WithMetrics
- 2 benchmarks: Throughput, Concurrency

### Backend (internal/backend/backend.go)
```go
// Backend with health state and connection tracking
b := backend.NewBackendWithWeight("host:port", 2)
b.IsHealthy() bool
b.SetHealthy(healthy bool, err error)
b.ActiveConnections() int64

// Dial returns tracked connection (auto-decrements on Close)
conn, err := b.Dial(ctx) // Returns trackedConn
defer conn.Close()       // Automatically decrements activeConns
```

### LoadBalancer (internal/loadbalancer/loadbalancer.go)
```go
// Interface for pluggable algorithms
type LoadBalancer interface {
    Next() (*backend.Backend, error)  // Returns next healthy backend
    Backends() []*backend.Backend
    AddBackend(b *backend.Backend)
    RemoveBackend(addr string) bool
}

// Implementations
lb := loadbalancer.NewRoundRobin(backends)         // Even distribution
lb := loadbalancer.NewWeightedRoundRobin(backends) // Respects weights (NGINX smooth)
lb := loadbalancer.NewLeastConnections(backends)   // Route to least busy
```

### Health Checker (internal/health/checker.go)
```go
// Threshold-based health checking
checker := health.NewChecker(backends, logger, health.CheckerConfig{
    Interval:           5 * time.Second,
    Timeout:            2 * time.Second,
    UnhealthyThreshold: 3,  // 3 consecutive failures = unhealthy
    HealthyThreshold:   2,  // 2 consecutive successes = healthy
    CheckType:          "tcp",
})
checker.Start(ctx)
defer checker.Stop()
```

### Circuit Breaker (internal/circuitbreaker/circuitbreaker.go)
```go
// Protect backends from cascading failures
cb := circuitbreaker.New(circuitbreaker.Config{
    FailureThreshold: 5,           // Opens after 5 failures
    SuccessThreshold: 1,           // Closes after 1 success in half-open
    Timeout:          30 * time.Second, // Wait before half-open
})

if err := cb.Allow(); err != nil {
    return err // Circuit is open
}
err := doRequest()
cb.RecordResult(err) // Track success/failure
```

### Rate Limiter (internal/ratelimiter/ratelimiter.go)
```go
// Token bucket rate limiting
limiter := ratelimiter.NewTokenBucket(100, 10) // 100 req/sec, burst 10

if limiter.Allow() {
    handleRequest()
} else {
    return 429 // Too Many Requests
}
```

### Metrics (internal/metrics/metrics.go)
```go
// Collect proxy statistics
collector := metrics.NewCollector()
collector.RecordConnection()
collector.RecordLatency(latency)
collector.RecordRequest(backend, success, latency, bytesSent, bytesReceived)

snap := collector.Snapshot() // Get current stats
```

### HTTP Router (internal/router/router.go)
```go
// Host and path-based routing with specificity scoring
router := router.New()

router.AddRoute(&router.Route{
    Name:       "api",
    Host:       "api.example.com",  // Optional host match
    PathPrefix: "/v1",              // Path prefix match
    Methods:    []string{"GET", "POST"}, // Optional method filter
    Handler:    apiHandler,
})

// Router implements http.Handler
http.ListenAndServe(":8080", router)

// Route matching priority:
// 1. Host match beats path-only match
// 2. Longer path prefix wins
// 3. Method-specific beats any-method
```

### Middleware Chain (internal/middleware/chain.go)
```go
// Standard Go middleware pattern
type Middleware func(http.Handler) http.Handler

// Chain wraps handler with middlewares (outer first, inner last)
handler := middleware.Chain(
    finalHandler,
    middleware.Recovery(logger),     // 1st: panic recovery
    middleware.Logging(logger),      // 2nd: request logging
    middleware.RateLimit(limiter),   // 3rd: rate limiting
    middleware.Headers(),            // 4th: proxy headers
)
```

### HTTP Proxy (internal/proxy/http.go)
```go
// Layer 7 reverse proxy with existing L4 components
proxy := NewHTTPProxy(lb, logger,
    WithHTTPCircuitBreaker(circuitbreaker.DefaultConfig()),
    WithHTTPMetrics(metrics.NewCollector()),
    WithHTTPTransport(DefaultTransport()),
)

// Implements http.Handler - use with router or middleware chain
```

### Traffic Splitter (internal/splitter/splitter.go)
```go
// Weighted traffic splitting for canary deployments
splitter, err := splitter.NewSplitter(
    &splitter.Target{Name: "prod", LoadBalancer: prodLB, Weight: 95},
    &splitter.Target{Name: "canary", LoadBalancer: canaryLB, Weight: 5},
)

target := splitter.Select()  // 95% prod, 5% canary
lb := target.LoadBalancer
backend, _ := lb.Next()

// Simpler pool-based API for common cases
simple := splitter.NewSimpleSplitter()
simple.AddPool(splitter.Pool{Name: "a", Weight: 50, Balancer: lbA})
simple.AddPool(splitter.Pool{Name: "b", Weight: 50, Balancer: lbB})
lb, poolName := simple.Route()  // Deterministic round-robin
```

### JWT Authentication (internal/auth/jwt.go)
```go
// Parse and validate JWT tokens (pure Go, no external deps)
validator := auth.NewValidator(auth.ValidatorConfig{
    SecretKey: []byte("your-secret-key"),  // HS256
    Issuer:    "auth.example.com",
    Audience:  "api.example.com",
    ClockSkew: 60 * time.Second,
})

token, err := validator.Validate(tokenString)
if err != nil {
    // auth.ErrTokenExpired, auth.ErrInvalidSignature, etc.
}

userID := token.Claims.Subject
roles := token.Claims.Custom["roles"]
```

### Auth Middleware (internal/middleware/auth.go)
```go
// JWT authentication middleware
authMiddleware := middleware.Auth(middleware.AuthConfig{
    Validator: validator,
    SkipAuth: func(r *http.Request) bool {
        return r.URL.Path == "/health"  // Public endpoints
    },
})

// Access claims in handlers
func handler(w http.ResponseWriter, r *http.Request) {
    claims := middleware.ClaimsFromContext(r.Context())
    if claims != nil {
        userID := claims.Subject
    }
}

// Optional auth (works with or without token)
optionalAuth := middleware.OptionalAuth(validator)
```

### CORS Middleware (internal/middleware/cors.go)
```go
// Cross-Origin Resource Sharing for browser requests
corsMiddleware := middleware.CORS(middleware.CORSConfig{
    AllowedOrigins:   []string{"https://app.example.com"},
    AllowedMethods:   []string{"GET", "POST", "PUT", "DELETE"},
    AllowedHeaders:   []string{"Authorization", "Content-Type"},
    AllowCredentials: true,
    MaxAge:           86400,  // Browser cache preflight for 24h
})

// Or use permissive defaults for development (not production!)
devCORS := middleware.CORS(middleware.DefaultCORSConfig())
```

## Patterns Applied

### 1. Non-Blocking Start() with Ready Channel
**Why**: Callers don't need goroutine wrapper, deterministic readiness, no time.Sleep hacks
```go
// OLD (error-prone):
go proxy.Start(ctx)
time.Sleep(50*time.Millisecond) // Race condition!

// NEW (industry standard):
proxy.Start(ctx)  // Non-blocking
<-proxy.Ready()   // Deterministic wait
```

### 2. Lifecycle State Machine
```
States: created → running → stopped
- Once stopped, cannot restart (create new instance)
- Prevents subtle bugs from channel reuse
```

### 3. WaitGroup for Connection Draining
```go
connWg.Add(1)         // Before goroutine
go handleConnection() // Decrement in defer
connWg.Wait()         // In Stop() - wait for all connections
```

### 4. Accept with Deadline for Cancellation
```go
// Accept() blocks forever, can't check ctx
// Solution: Set 1s deadline, loop with ctx check
tcpListener.SetDeadline(time.Now().Add(1*time.Second))
conn, err := listener.Accept()
if netErr.Timeout() { continue } // Check ctx and loop
```

### 5. trackedConn Wrapper for Automatic Cleanup (Milestone 2)
**Problem**: Need to track active connections per backend, but callers forget to decrement.
**Solution**: Wrap net.Conn with automatic decrement on Close().
```go
type trackedConn struct {
    net.Conn
    backend *Backend
    closed  atomic.Bool
}

func (c *trackedConn) Close() error {
    if c.closed.CompareAndSwap(false, true) {
        c.backend.activeConns.Add(-1)  // Auto-decrement
    }
    return c.Conn.Close()
}
```

### 6. CloseWrite Interface for Duck Typing (Milestone 2)
**Problem**: trackedConn embeds net.Conn but blocks access to *net.TCPConn.CloseWrite().
**Solution**: Add CloseWrite() to wrapper, use interface in caller.
```go
// In trackedConn
func (c *trackedConn) CloseWrite() error {
    if tcpConn, ok := c.Conn.(*net.TCPConn); ok {
        return tcpConn.CloseWrite()
    }
    return nil
}

// In proxy - works with both *net.TCPConn and trackedConn
if cw, ok := dst.(interface{ CloseWrite() error }); ok {
    cw.CloseWrite()
}
```

### 7. Threshold-Based Health Transitions (Milestone 2)
**Problem**: Single check failure marking backend unhealthy causes flapping.
**Solution**: Require N consecutive failures/successes before state change.
```go
if err != nil {
    c.failureCount[addr]++
    c.successCount[addr] = 0
    if c.failureCount[addr] >= c.config.UnhealthyThreshold {
        b.SetHealthy(false, err)
    }
} else {
    c.failureCount[addr] = 0
    c.successCount[addr]++
    if c.successCount[addr] >= c.config.HealthyThreshold && !wasHealthy {
        b.SetHealthy(true, nil)
    }
}
```

### 8. Circuit Breaker State Machine (Milestone 3)
**Pattern**: State machine with three states controls request flow.
```
CLOSED --[failures > threshold]--> OPEN
OPEN   --[timeout elapsed]-------> HALF-OPEN
HALF-OPEN --[success]------------> CLOSED
HALF-OPEN --[failure]------------> OPEN
```
**Why**: Fail fast on known-bad backends, give time to recover.

### 9. Token Bucket Rate Limiting (Milestone 3)
**Algorithm**: Bucket holds tokens, refills at constant rate.
```go
func (tb *TokenBucket) AllowN(n int) bool {
    tb.mu.Lock()
    defer tb.mu.Unlock()
    
    tb.refill() // Add tokens based on elapsed time
    
    if tb.tokens >= float64(n) {
        tb.tokens -= float64(n)
        return true
    }
    return false
}
```
**Why**: Allows bursts (up to bucket capacity), smooth rate over time.

### 10. Double-Checked Locking for Maps (Milestone 3)
**Problem**: Need to lazily create entries in concurrent map.
**Solution**: Read lock first, then write lock with re-check.
```go
func (c *Collector) getOrCreateBackendMetrics(addr string) *BackendMetrics {
    c.mu.RLock()
    m, ok := c.backendMetrics[addr]
    c.mu.RUnlock()
    if ok {
        return m
    }

    c.mu.Lock()
    defer c.mu.Unlock()
    // Re-check after acquiring write lock
    if m, ok := c.backendMetrics[addr]; ok {
        return m
    }
    m = &BackendMetrics{}
    c.backendMetrics[addr] = m
    return m
}
```

### 11. Functional Options Pattern (Milestone 4)
**Problem**: Proxy has many optional features (rate limiter, circuit breaker, metrics).
**Solution**: Use functional options for clean, extensible API.
```go
type Option func(*TCPProxy)

func WithRateLimiter(rl *ratelimiter.TokenBucket) Option {
    return func(p *TCPProxy) { p.rateLimiter = rl }
}

func WithCircuitBreaker(config circuitbreaker.Config) Option {
    return func(p *TCPProxy) {
        p.circuitBreakers = make(map[string]*circuitbreaker.CircuitBreaker)
        p.cbConfig = config
    }
}

// Usage: Clean, readable, optional parameters
proxy := NewTCPProxy(addr, lb, logger,
    WithRateLimiter(rl),
    WithCircuitBreaker(cbConfig),
    WithMetrics(collector),
)
```
**Why**: Avoids config struct explosion, backward compatible, self-documenting.

### 12. NGINX Smooth Weighted Round-Robin (Milestone 4)
**Problem**: Simple weighted RR sends bursts to high-weight backends.
**Solution**: NGINX smooth algorithm distributes evenly.
```go
// Each request:
// 1. Add effective weight to current weight for all backends
// 2. Select backend with highest current weight
// 3. Subtract total weight from selected backend's current weight

func (wrr *WeightedRoundRobin) Next() (*backend.Backend, error) {
    var selected *weightedBackend
    totalWeight := 0

    for _, wb := range wrr.backends {
        if !wb.backend.IsHealthy() { continue }
        wb.currentWeight += wb.effectiveWeight
        totalWeight += wb.effectiveWeight
        if selected == nil || wb.currentWeight > selected.currentWeight {
            selected = wb
        }
    }
    selected.currentWeight -= totalWeight
    return selected.backend, nil
}
```
**Example**: Weights [5,1,1] produces A,A,B,A,A,C,A (smooth) not AAAAA,B,C (burst).

## Key Learnings

### 1. Blocking vs Non-Blocking Start()
| Aspect | Blocking | Non-Blocking |
|--------|----------|--------------|
| Caller complexity | Must wrap in goroutine | Direct call |
| Readiness signal | time.Sleep (race-prone) | Channel (deterministic) |
| Error handling | Channel/return | Immediate return |
| Used by | gRPC, http.Server | k8s controller-runtime, NATS |

**Decision**: Non-blocking is better for testability and API ergonomics.

### 2. Proxy Lifecycle Not Restartable
Why not support restart?
- `ready`/`done` channels closed after first use
- Resetting state is complex and error-prone
- Industry pattern: Create new instance instead
- Simpler code, fewer bugs

### 3. Accept Loop Timeout Pattern
Problem: `Accept()` blocks indefinitely - can't check context cancellation.
Solution: Set 1-second deadline, check ctx on timeout, continue loop.
```go
for {
    select { case <-ctx.Done(): return; default: }
    listener.SetDeadline(time.Now().Add(1*time.Second))
    conn, err := listener.Accept()
    if netErr.Timeout() { continue } // Timeout = check ctx again
    ...
}
```

### 4. Test Without time.Sleep
Before: `time.Sleep(50ms)` - fragile, race conditions
After: `<-proxy.Ready()` - deterministic, faster tests

### 5. Interface Satisfaction for Optional Methods (Milestone 2)
When wrapping types that need to expose underlying methods:
- Type assertion to concrete type fails for wrappers
- Add method to wrapper that delegates to underlying
- Use small interface for duck typing: `interface{ CloseWrite() error }`

### 6. Atomic vs Mutex for Counters (Milestone 2)
| Use Case | Choose |
|----------|--------|
| Simple increment/decrement | atomic.Int64 |
| Read-modify-write (conditionally) | Mutex |
| Multiple related fields | Mutex (consistency) |
| Hot path (high contention) | atomic (lock-free) |

`trackedConn.closed` uses `atomic.Bool.CompareAndSwap` for double-close prevention.

### 7. Connection Tracking Design (Milestone 2)
Options considered:
1. **Caller tracks** - Error-prone (forget to decrement)
2. **Wrapper tracks** - Automatic, zero-cost to caller ✅
3. **Separate pool** - More complex, overkill for our needs

Wrapper pattern: Return `trackedConn` from `Dial()`, automatically decrement on `Close()`.

### 8. Circuit Breaker Design (Milestone 3)
| State | Behavior | Transition Out |
|-------|----------|----------------|
| CLOSED | All requests pass | failures > threshold → OPEN |
| OPEN | All requests rejected | timeout elapsed → HALF-OPEN |
| HALF-OPEN | One test request | success → CLOSED, failure → OPEN |

**Key insight**: The "window" for counting failures prevents old failures from triggering opens.

### 9. Token Bucket vs Sliding Window (Milestone 3)
| Algorithm | Burst Handling | Memory | Complexity |
|-----------|----------------|--------|------------|
| Token Bucket | Allows bursts up to capacity | O(1) | Simple |
| Sliding Window | Smoother, no bursts | O(n) | More complex |

**Decision**: Token bucket is simpler and sufficient for most use cases.

### 10. Metrics Atomics Strategy (Milestone 3)
- **Counters (hot path)**: Use `atomic.Int64` - lock-free, fast
- **Maps (per-backend)**: Use `sync.RWMutex` - needs coordination
- **Histograms**: Use atomic array - lock-free bucket increments

## Design Decisions

### D1: Non-Blocking Start() over Blocking
**Rationale**: Industry standard (k8s, NATS), cleaner tests, better API
**Trade-off**: Slightly more complex internal state management

### D2: Single Backend (Week 1)
**Rationale**: Focus on TCP fundamentals before load balancing
**Future**: Week 2 adds multiple backends with health checks

### D3: Not Restartable After Stop()
**Rationale**: Avoid channel reuse bugs, simpler state machine
**Trade-off**: Must create new proxy instance for restart

## Progress Tracking

### Milestone 1: TCP Fundamentals & Basic Proxy ✅ COMPLETE

**Setup** ✅
- [x] Project structure created
- [x] Dependencies installed
- [x] Code scaffolded with learning-focused comments
- [x] Compiles successfully

**Implementation** ✅
- [x] Config loading (internal/config/config.go)
- [x] Signal handling (cmd/gogate/main.go)
- [x] TCP proxy Start() - Non-blocking with Ready() channel
- [x] TCP proxy handleConnection() - Bidirectional forwarding
- [x] TCP proxy copyWithHalfClose() - Copy with half-close semantics
- [x] TCP proxy Stop() - Graceful shutdown with connection draining
- [x] Manual testing verified
- [x] Automated tests (7 tests, 2 benchmarks, race-free)

**Learning Checkpoints** ✅
- [x] Why two goroutines are needed for full-duplex copy
- [x] What half-close is and when it matters
- [x] TCP connection lifecycle (SYN, ACK, FIN)
- [x] Resource cleanup strategies (defer, WaitGroup)
- [x] Graceful shutdown mechanics (listener close, drain, timeout)
- [x] Non-blocking Start() pattern and why it's preferred


### Milestone 2: Load Balancing & Health Checks ✅ COMPLETE

**Setup** ✅
- [x] Backend abstraction created (internal/backend/backend.go)
- [x] LoadBalancer interface defined (internal/loadbalancer/loadbalancer.go)
- [x] Health checker created (internal/health/checker.go)
- [x] Config updated for multiple backends

**Implementation** ✅
- [x] YAML config with multiple backends
- [x] Backend abstraction (addr, weight, health state, connection tracking)
- [x] Round-robin load balancer (skips unhealthy)
- [x] Least-connections load balancer (atomic counter based)
- [x] Active health checks (TCP connect, configurable thresholds)
- [x] trackedConn wrapper for automatic connection counting
- [x] All tests passing (24 total: 6 backend, 10 loadbalancer, 7 proxy, 1 health)

**Learning Checkpoints** ✅
- [x] Interface-based load balancer design (strategy pattern)
- [x] Atomic operations for concurrent counters
- [x] Threshold-based health state transitions
- [x] Connection tracking via wrapper type
- [x] CloseWrite interface for half-close through wrappers

**Patterns Applied**:
- trackedConn wrapper: Automatic connection counting on Close()
- CloseWrite interface: Duck typing for half-close support
- Threshold-based health: Avoid flapping with consecutive success/failure counts

---

### Milestone 3: Resilience & Observability ✅ COMPLETE

**Implementation** ✅
- [x] Circuit breaker (states: closed, open, half-open)
- [x] Rate limiter (token bucket algorithm)
- [x] Metrics collector (connections, latency histogram, per-backend stats)
- [x] All tests passing (22 new tests)

**Learning Checkpoints** ✅
- [x] Circuit breaker state machine design
- [x] Token bucket rate limiting algorithm
- [x] Atomic operations vs mutex for metrics
- [x] Histogram design for latency tracking

**Patterns Applied**:
- State machine for circuit breaker (closed/open/half-open)
- Token bucket for rate limiting (allows bursts, smooth refill)
- Double-checked locking for backend metrics map

---

### Milestone 4: Integration & Weighted Load Balancing ✅ COMPLETE

**Implementation** ✅
- [x] Weighted round-robin load balancer (NGINX smooth WRR algorithm)
- [x] Integrate circuit breaker into proxy (per-backend, lazy creation)
- [x] Integrate rate limiter into proxy (global, token bucket)
- [x] Integrate metrics collector into proxy (connections, bytes, latency)
- [x] Add rate_limit, circuit_breaker, admin config to YAML
- [x] Admin HTTP endpoint (/stats, /backends, /health)
- [x] Integration tests (4 new: rate limiter, circuit breaker, metrics, weighted RR)
- [x] All tests passing (57 total across 7 packages)

**Learning Checkpoints** ✅
- [x] Functional options pattern for clean optional dependencies
- [x] NGINX smooth weighted round-robin algorithm
- [x] Per-backend lazy circuit breaker creation
- [x] Admin HTTP server with graceful shutdown

**Patterns Applied**:
- Functional Options: `WithRateLimiter()`, `WithCircuitBreaker()`, `WithMetrics()`
- Smooth WRR: Prevents burst traffic to high-weight backends
- Lazy Initialization: Circuit breakers created on first use per backend
- Double-checked locking: Thread-safe circuit breaker map access

**Notes:**
- Milestone 3 built standalone components
- Milestone 4 integrated them into the actual proxy
- TCP proxy is now feature-complete

---

### Milestone 5: HTTP Layer & Middleware ✓
- [x] HTTP reverse proxy (net/http based, custom implementation)
- [x] Host/path-based routing (Router with route matching)
- [x] Middleware chain architecture (func(http.Handler) http.Handler pattern)
- [x] Logging middleware (structured request/response logging)
- [x] Rate limit middleware (wrap existing rate limiter for HTTP)
- [x] Recovery middleware (panic recovery)
- [x] Headers middleware (X-Forwarded-For, X-Real-IP, etc.)
- [x] Request ID middleware (X-Request-ID generation/preservation)
- [x] Strip prefix middleware (path rewriting)
- [ ] HTTP health checks (extend health checker for HTTP endpoints) - defer to M6
- [ ] Retry with exponential backoff - defer to M8

**Architecture:**
```
HTTP Request → Router → Middleware Chain → HTTP Proxy → LoadBalancer → Backend
```

**Notes:**
- Built on top of existing L4 infrastructure (LoadBalancer, CircuitBreaker, Metrics)
- Standard Go middleware pattern for composability
- Route-specific middleware support ready (handler per route)
- HTTP health checks deferred - current TCP health checks work for HTTP backends

---

### Milestone 6: Traffic Splitting & JWT Authentication 🎯 CURRENT

**Architecture Design:**

```
                                 ┌─────────────────────────────────────────────┐
                                 │            JWT Validation Flow              │
                                 │                                             │
Request ──► Auth Middleware ─────┤  1. Extract token from Authorization header │
     │            │              │  2. Base64 decode header.payload.signature  │
     │            │              │  3. Verify signature with public key        │
     │            │              │  4. Check claims: exp, iss, aud             │
     │            ▼              │  5. Pass claims via context to handlers     │
     │    ┌───────────────┐      └─────────────────────────────────────────────┘
     │    │  JWT Valid?   │
     │    └───────┬───────┘
     │       yes/ \no
     │         /   \
     │        ▼     ▼
     │    Continue  401 Unauthorized
     │        │
     ▼        ▼
┌─────────────────────────────────────────────────────────────────────────────┐
│                        Traffic Splitting Decision                            │
│                                                                              │
│  Route config:                                                               │
│    targets:                                                                  │
│      - backend_pool: "production"   weight: 90                               │
│      - backend_pool: "canary"       weight: 10                               │
│                                                                              │
│  Algorithm: weighted random selection                                        │
│    1. Sum weights (100)                                                      │
│    2. Generate random 0-99                                                   │
│    3. If random < 90 → production, else → canary                            │
└─────────────────────────────────────────────────────────────────────────────┘
```

**Implementation Tasks:**
- [ ] Traffic Splitter (internal/splitter/splitter.go)
  - Weighted random selection between backend pools
  - Config-based target definitions
  - Thread-safe random number generation
- [ ] JWT Parser (internal/auth/jwt.go)
  - Parse without external dependencies (only crypto/*)
  - Support HS256 (HMAC-SHA256) and RS256 (RSA-SHA256)
  - Base64URL decode header/payload/signature
  - Claims struct with standard fields (exp, iat, iss, aud, sub)
- [ ] JWT Validator (internal/auth/validator.go)
  - Signature verification
  - Expiration check (exp claim)
  - Issuer validation (iss claim)
  - Audience validation (aud claim)
  - Clock skew tolerance
- [ ] Auth Middleware (internal/middleware/auth.go)
  - Extract Bearer token from Authorization header
  - Call validator, return 401 on failure
  - Store claims in context for downstream handlers
  - Support route-level auth requirements (public vs protected)
- [ ] CORS Middleware (internal/middleware/cors.go)
  - Configurable allowed origins, methods, headers
  - Preflight (OPTIONS) request handling
  - Credential support

**JWT Structure (for reference):**
```
Header.Payload.Signature (Base64URL encoded)

Header: {"alg": "RS256", "typ": "JWT"}
Payload: {"sub": "user123", "exp": 1700000000, "iss": "auth.example.com"}
Signature: RSASSA-PKCS1-v1_5(SHA256(base64url(header) + "." + base64url(payload)))
```

**Why no external deps for JWT?**
- Learning exercise: Understand JWT internals
- Go stdlib has everything needed (crypto/rsa, crypto/hmac, encoding/base64)
- Production would use github.com/golang-jwt/jwt, but we learn more building it

**Notes:**
- JWT without external dependencies (crypto/ecdsa, crypto/rsa)
- Support RS256 and HS256 algorithms
- Claims validation (exp, iss, aud)

---

### Milestone 7: Connection Pooling & Performance ✅ COMPLETE
- [x] sync.Pool for byte buffers (reduce allocations)
- [x] TCP connection pooling to backends (reuse connections)
- [x] HTTP connection pooling (http.Transport settings)
- [x] Keep-alive management
- [x] Idle connection timeout
- [x] Benchmarking suite (measure improvements)
- [ ] Memory profiling with pprof (deferred to M11)

**Notes:**
- Per-backend TCP pools with idle timeout and TTL
- sync.Pool for 32KB buffers in TCP proxy
- 114+ tests passing

---

### Milestone 8: Service Discovery & Hot Reload ✅ COMPLETE
- [x] Service registration API (POST /v1/services)
- [x] Heartbeat endpoint (PUT /v1/services/{id}/heartbeat)
- [x] TTL-based deregistration (auto-remove stale services)
- [x] Service metadata and tags
- [x] Watch API for config changes (channel-based Subscribe/Unsubscribe)
- [x] Hot reload via SIGHUP (reload config without restart)
- [x] LoadBalancer.UpdateBackends() for dynamic backend updates
- [ ] Service discovery client SDK (future enhancement)

**Implementation:**
- `internal/registry/registry.go`: Service, Registry, Event, Config structs
- `internal/registry/handler.go`: REST API handlers
- `internal/registry/registry_test.go`: 28+ comprehensive tests
- `cmd/gogate/main.go`: Registry server, SIGHUP handler, watchRegistryChanges

**Key Patterns:**
- TTL expiration via background cleanup goroutine
- Non-blocking channel sends for watch mechanism
- SIGHUP signal for zero-downtime config reload
- 171 tests passing (57 new)

---

### Milestone 9: Prometheus & Observability
- [ ] Prometheus metrics endpoint (/metrics)
- [ ] Counter: gogate_connections_total, gogate_requests_total
- [ ] Gauge: gogate_connections_active, gogate_backend_health
- [ ] Histogram: gogate_request_duration_seconds
- [ ] Circuit breaker state metric
- [ ] Rate limiter rejection metric
- [ ] Request tracing headers (X-Request-ID propagation)
- [ ] Grafana dashboard JSON
- [ ] Structured logging improvements (correlation IDs)

**Notes:**
- Use prometheus/client_golang
- Pre-defined buckets for latency histogram
- Label design for backend, route, method, status

---

### Milestone 10: Docker, K8s & Production Readiness
- [ ] Dockerfile (multi-stage build, minimal image)
- [ ] docker-compose for local dev (proxy + backends)
- [ ] Helm chart for K8s (Deployment, Service, ConfigMap)
- [ ] ServiceMonitor for Prometheus Operator
- [ ] Health/readiness probes configuration
- [ ] Resource limits and requests
- [ ] k6 load tests (10K concurrent connections target)
- [ ] Performance benchmarks documented
- [ ] README polish with examples
- [ ] Demo video

**Load Test Targets:**
- 10K concurrent TCP connections
- 5K HTTP requests/second
- p99 latency < 10ms
- Memory < 500MB under load

---

## Questions & Blockers

<!-- Track questions to research or ask -->

## Design Decisions

### D1: Non-Blocking Start() over Blocking
**Rationale**: Industry standard (k8s, NATS), cleaner tests, better API
**Trade-off**: Slightly more complex internal state management

### D2: Single Backend (Week 1)
**Rationale**: Focus on TCP fundamentals before load balancing
**Future**: Week 2 adds multiple backends with health checks

### D3: Not Restartable After Stop()
**Rationale**: Avoid channel reuse bugs, simpler state machine
**Trade-off**: Must create new proxy instance for restart
