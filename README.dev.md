# GoGate - L4/L7 Proxy & API Gateway with Service Discovery

A production-grade proxy supporting raw TCP (L4) and HTTP (L7) with dynamic service discovery, traffic management, and full observability.

## Architecture

```
                                    ┌──────────────────────────────────────────┐
                                    │              GoGate                      │
                                    │                                          │
┌──────────┐                        │  ┌──────────┐    ┌──────────────────┐    │
│  Client  │─────TCP/HTTP──────────►│  │ Listener │───►│ Protocol Detect  │    │
└──────────┘                        │  └──────────┘    └────────┬─────────┘    │
                                    │                           │              │
                                    │              ┌────────────┴────────────┐ │
                                    │              ▼                         ▼ │
                                    │     ┌─────────────┐           ┌─────────┐│
                                    │     │  L4 Proxy   │           │L7 Proxy ││
                                    │     │  (raw TCP)  │           │ (HTTP)  ││
                                    │     └──────┬──────┘           └────┬────┘│
                                    │            │                       │     │
                                    │            │    ┌──────────────────┤     │
                                    │            │    │ Middleware Stack │     │
                                    │            │    ├──────────────────┤     │
                                    │            │    │ • Rate Limiter   │     │
                                    │            │    │ • JWT Auth       │     │
                                    │            │    │ • Circuit Breaker│     │
                                    │            │    │ • Traffic Split  │     │
                                    │            │    └──────────────────┘     │
                                    │            │                       │     │
                                    │            ▼                       ▼     │
                                    │     ┌─────────────────────────────────┐  │
                                    │     │        Load Balancer            │  │
                                    │     │  (round-robin / least-conn)     │  │
                                    │     └───────────────┬─────────────────┘  │
                                    │                     │                    │
                                    │     ┌───────────────┴───────────────┐    │
                                    │     │      Service Registry         │    │
                                    │     │  (heartbeat + health checks)  │    │
                                    │     └───────────────────────────────┘    │
                                    └─────────────────────┬────────────────────┘
                                                          │
                        ┌─────────────────────────────────┼─────────────────────────────────┐
                        ▼                                 ▼                                 ▼
                 ┌────────────┐                    ┌────────────┐                    ┌────────────┐
                 │ Backend A  │                    │ Backend B  │                    │ Backend C  │
                 │ (v1: 80%)  │                    │ (v2: 20%)  │                    │ (standby)  │
                 └────────────┘                    └────────────┘                    └────────────┘
```

## Features

### L4 Layer (TCP) ✅ Milestone 1-2 Complete
- [x] Raw TCP proxying with bidirectional copy
- [x] Multiple load balancing algorithms (round-robin, weighted RR, least-connections)
- [x] Active health checks (TCP dial, configurable thresholds)
- [x] Graceful shutdown with connection draining
- [x] Connection tracking (per-backend active connections)
- [ ] Connection pooling with reuse (Milestone 7)
- [ ] sync.Pool for buffer reuse (Milestone 7)

### L7 Layer (HTTP) 🚧 Milestone 5
- [ ] HTTP/1.1 reverse proxy
- [ ] Path & host-based routing
- [ ] Request/response header manipulation
- [ ] Middleware chain architecture
- [ ] HTTP health checks

### Traffic Management ✅ Milestone 3-4 Complete
- [x] Rate limiting (token bucket algorithm)
- [x] Circuit breaker pattern (closed/open/half-open states)
- [x] Weighted traffic distribution (NGINX smooth WRR)
- [ ] Retry with exponential backoff (Milestone 5)
- [ ] Traffic splitting for canary deployments (Milestone 6)
- [ ] A/B routing via headers (Milestone 6)

### Security (Milestone 6)
- [ ] JWT token validation
- [ ] TLS termination
- [ ] mTLS between proxy and backends

### Service Discovery (Milestone 8)
- [ ] Service registration API
- [ ] Heartbeat-based health (TTL expiration)
- [ ] Service metadata and tags
- [ ] Watch API for config updates

### Observability
- [x] Metrics collector (connections, latency histogram, per-backend stats)
- [x] Structured JSON logging (slog)
- [x] Admin HTTP endpoint (/stats, /backends, /health)
- [ ] Prometheus metrics endpoint (Milestone 9)
- [ ] Request tracing headers (Milestone 9)
- [ ] Grafana dashboard (Milestone 9)

### Operations
- [x] YAML configuration with defaults
- [x] Graceful shutdown (SIGINT/SIGTERM)
- [x] Non-blocking server start with Ready() channel
- [ ] Hot reload via SIGHUP (Milestone 8)
- [ ] Dockerfile (multi-stage build) (Milestone 10)
- [ ] docker-compose for local dev (Milestone 10)
- [ ] Helm chart for K8s (Milestone 10)
- [ ] k6 load tests (Milestone 10)

---

## Progress & Development Notes

**See [PROGRESS.md](./PROGRESS.md)** for detailed session-by-session progress, implementation notes, patterns learned, and design decisions.

### Current Status: Milestone 5 - HTTP Layer

| Milestone | Focus | Status |
|-----------|-------|--------|
| 1 | TCP Fundamentals & Basic Proxy | ✅ Complete |
| 2 | Load Balancing & Health Checks | ✅ Complete |
| 3 | Resilience (Circuit Breaker, Rate Limiter, Metrics) | ✅ Complete |
| 4 | Integration & Weighted Load Balancing | ✅ Complete |
| 5 | HTTP Layer & Middleware | 🚧 Current |
| 6 | Traffic Splitting & JWT Auth | ⏳ Planned |
| 7 | Connection Pooling & Performance | ⏳ Planned |
| 8 | Service Discovery & Hot Reload | ⏳ Planned |
| 9 | Prometheus & Observability | ⏳ Planned |
| 10 | Docker, K8s & Production Readiness | ⏳ Planned |

---

## Configuration Reference

```yaml
# Full config example
server:
  tcpListen: ":8080"      # L4 proxy port
  httpListen: ":8081"     # L7 proxy port  
  adminListen: ":9090"    # Admin API port
  metricsListen: ":9091"  # Prometheus metrics

logging:
  level: info
  format: json

registry:
  ttl: 30s
  cleanupInterval: 10s

# Static backends (can also use service discovery)
backends:
  api-v1:
    addresses:
      - "localhost:9001"
      - "localhost:9002"
    healthCheck:
      type: http
      path: /health
      interval: 10s
      timeout: 2s
      unhealthyThreshold: 3

  api-v2:
    addresses:
      - "localhost:9003"
    tags: ["canary"]

routes:
  - name: api-route
    match:
      pathPrefix: /api
    backends:
      - name: api-v1
        weight: 90
      - name: api-v2
        weight: 10
    middleware:
      - rateLimit:
          requests: 1000
          window: 1m
      - auth:
          type: jwt
          issuer: "https://auth.example.com"
      - circuitBreaker:
          threshold: 5
          timeout: 30s
          
  - name: tcp-passthrough
    listen: ":5432"
    type: tcp
    backend: postgres-pool
    loadBalancer: least-connections
```

---

## Development

```bash
# Run locally
go run ./cmd/gogate -config config.yaml

# Run with hot reload (development)
go run ./cmd/gogate -config config.yaml -dev

# Build
go build -o bin/gogate ./cmd/gogate

# Test
go test ./... -v

# Benchmark
go test ./... -bench=. -benchmem

# Load test
k6 run scripts/loadtest.js

# Docker
docker build -t gogate:latest -f deployments/docker/Dockerfile .
docker-compose -f deployments/docker-compose.yaml up

# Kubernetes
helm install gogate ./deployments/helm/gogate
```

---

## Learning Resources

### TCP/Networking
- [The TCP/IP Guide](http://www.tcpipguide.com/) - Deep dive into TCP
- [Beej's Guide to Network Programming](https://beej.us/guide/bgnet/) - Classic
- Go's `net` package source code

### Proxy Patterns
- HAProxy documentation (architecture section)
- Envoy proxy documentation
- [How Netflix Does Failovers](https://netflixtechblog.com/)

### Rate Limiting
- [Token Bucket Algorithm](https://en.wikipedia.org/wiki/Token_bucket)
- [Leaky Bucket vs Token Bucket](https://www.educative.io/answers/what-is-the-leaky-bucket-algorithm)

### Circuit Breaker
- [Martin Fowler's Circuit Breaker](https://martinfowler.com/bliki/CircuitBreaker.html)
- [Release It!](https://pragprog.com/titles/mnee2/release-it-second-edition/) - Must read



I was thinking:
- what about udp ? can we add udp proxying also ? i'ven't seen this in the code/architecture out there , in fact i never happen to use upd, less udp proxying in real world scenarios.    