# Load Testing & Monitoring GoGate

This guide walks you through running a comprehensive load test of the gogate API gateway while monitoring metrics in Grafana.

## Architecture

```
┌─────────────────────────────────────────────┐
│         Load Test (k6)                      │
│  - Steady state, ramp-up, spike patterns   │
└──────────────────┬──────────────────────────┘
                   │
                   ▼
┌─────────────────────────────────────────────┐
│      GoGate API Gateway (L4/L7)             │
│  - Rate limiter, Circuit breaker           │
│  - Load balancing, Connection pooling      │
│  - Prometheus /metrics endpoint             │
└──────────────────┬──────────────────────────┘
                   │
        ┌──────────┴──────────┐
        ▼                     ▼
    ┌────────┐           ┌────────┐
    │Backend1│           │Backend2│
    │(httpbin)│           │(httpbin)│
    └────────┘           └────────┘
        │
        ▼
┌─────────────────────────────────────────────┐
│       Prometheus (time-series DB)           │
│  - Scrapes /metrics every 5 seconds         │
│  - Retains 15d of history                   │
└──────────────────┬──────────────────────────┘
                   │
                   ▼
┌─────────────────────────────────────────────┐
│    Grafana (Visualization)                  │
│  - Dashboards with key metrics              │
│  - Alerting rules (optional)                │
└─────────────────────────────────────────────┘
```

## Quick Start

### 1. Start the Full Stack

```bash
cd /Users/basit/projects/learning-go-v1/gogate

# Build and run all services
docker compose up
```

Wait for all services to be ready:
- **gogate** - listens on 8080 (HTTP), 8081 (TCP)
- **prometheus** - available at http://localhost:9090
- **grafana** - available at http://localhost:3000
- **backend-1, backend-2** - sample httpbin services

### 2. Access Grafana

1. Open http://localhost:3000
2. Login: `admin` / `admin`
3. Go to **Dashboards** → **GoGate API Gateway**

The dashboard auto-refreshes every 5 seconds and shows:
- **P99/P95/P50 Latency** - Request response times (upper chart)
- **Request Rate** - HTTP and TCP requests per second (upper right)
- **Rate Limiter Decisions** - Allowed vs rejected requests (lower left)
- **Circuit Breaker States** - Status per backend (lower right, color-coded)
- **Backend Health** - Which backends are up/down (bottom left)
- **Connection Pool Hit Rate** - % reused connections (bottom right)
- **TCP Throughput** - Bytes/sec transferred (bottom, full width)

### 3. Run Load Tests

Open another terminal:

```bash
cd /Users/basit/projects/learning-go-v1/gogate/k6

# Run steady-state load test (1000 req/s for 3 minutes)
k6 run load-test.js

# Or run rate limiter test (triggers rejection at limits)
k6 run rate-limit-test.js

# Or run circuit breaker test (triggers failures)
k6 run circuit-breaker-test.js

# Or run TCP test
k6 run tcp-test.js

# Or run long soak test (30 minutes)
k6 run soak-test.js --duration 30m
```

### 4. Watch Metrics in Real-Time

While the load test runs, watch Grafana update every 5 seconds:
- **Latency spikes** when request rate increases
- **Rate limiter rejections** when threshold exceeded
- **Circuit breaker opens** when backend fails
- **Pool hit rate** improves as connections reuse
- **Throughput** reflects bytes flowing through proxy

---

## What Each Metric Tells You

### P99 Latency
- **What it is**: 99th percentile request response time (microseconds → ms)
- **Why it matters**: Tells you tail latency; most users < P99, but 1% wait longer
- **Healthy range**: < 100ms for API gateway
- **Red flag**: Spikes during load increase = system struggling

**Example reading:**
- P50: 10ms (50% of requests finish in 10ms)
- P95: 25ms (95% finish in 25ms, 5% take longer)
- P99: 150ms (99% finish in 150ms, 1% take longer)
- **Interpretation**: Most requests are fast, but outliers are slow

### Request Rate
- **What it is**: Requests per second (HTTP + TCP)
- **Why it matters**: Measures throughput, helps identify load pattern
- **During load test**: Should increase steadily (ramp-up), hold steady, then drop

### Rate Limiter Decisions
- **Allowed**: Requests that passed rate limit (green)
- **Rejected**: Requests blocked because limit exceeded (red)
- **Why it matters**: Shows if your rate limit config is working

**Example:** If you set limit to 500 req/s and load reaches 800 req/s:
- Allowed: 500 req/s (green bar)
- Rejected: 300 req/s (red bar)

### Circuit Breaker States
- **CLOSED (green)**: Backend is healthy, requests flow through
- **OPEN (red)**: Backend is failing, requests rejected immediately (fail-fast)
- **HALF-OPEN (yellow)**: Testing if backend recovered, allowing some requests

**Why it matters**: Shows if failover is working; when a backend fails, CB opens immediately instead of hammering it.

### Backend Health
- **UP (green)**: Backend responding to checks
- **DOWN (red)**: Backend not responding, being skipped

### Connection Pool Hit Rate
- **What it is**: % of requests that reused a pooled connection
- **Why it matters**: Higher = better; reused connections have lower latency and overhead
- **Good rate**: > 80% during steady load
- **Poor rate**: < 50% means not reusing connections (connections closing too fast)

### TCP Throughput
- **What it is**: Bytes per second flowing through the proxy (both directions)
- **Why it matters**: Measures how much data moved; helps spot bottlenecks

---

## Load Test Scenarios

### load-test.js (Recommended for Dashboard)
```
Ramp: 0 → 500 req/s over 1 min
Steady: 500 req/s for 2 minutes
Spike: Jump to 1000 req/s for 30 sec
Cool: Drop to 200 req/s for 1 min
```

**What to watch:**
1. **Ramp phase**: Latency should stay flat, rate limiter allows all
2. **Steady phase**: Requests smooth, pool hits maximize
3. **Spike phase**: Latency jumps, rate limiter starts rejecting, CB might open
4. **Cool phase**: System recovers, queue drains

### rate-limit-test.js
```
Starts at 100 req/s, ramps up to 1000+ req/s
```

**What to watch:**
- Rejections start when hitting your configured limit
- Allowed requests cap out
- Latency for allowed requests stays low (CB protecting backend)

### circuit-breaker-test.js
```
Normal load + simulated 500 errors on one backend
```

**What to watch:**
- When errors hit threshold, CB opens (red state in dashboard)
- Requests immediately fail instead of slow timeout
- Load re-routes to healthy backend
- After error window closes, CB tries half-open (yellow), then closes again

### soak-test.js (30+ minutes)
```
Sustained 200 req/s with realistic think time
```

**What to watch:**
- Memory stability (no leaks)
- Latency consistency (no degradation over time)
- Pool hit rate trends (should stay high)
- Any connection limit exhaustion

---

## Key Insights During Load

### Scenario: Load Ramps from 100 → 1000 req/s

**Expected behavior:**
```
Request Rate:        100 → 200 → 300 → ... → 1000 req/s
P99 Latency:         10ms → 12ms → 15ms → ... → 150ms
Rate Limiter:        All allowed → Some rejected → Mostly rejected
Circuit Breaker:     Closed → Closed → ... → Opens after failures
Pool Hit Rate:       Rising → 85% → High percentage
```

**Good signs:**
- Latency increases gradually, not suddenly
- Rate limiter gracefully rejects overload
- Circuit breaker prevents cascading failures
- Pool hit rate stabilizes high

**Bad signs:**
- Latency spikes suddenly (degradation)
- No rate limiting (all requests enqueue, then timeout)
- Circuit breaker slow to detect/recover
- Pool hit rate drops (connection churn)

---

## Interpreting the Dashboard

### All Metrics Normal
✅ Everything smooth, green lights
- Use this as your baseline for healthy behavior

### Latency Spike
🔍 Check:
1. Did request rate increase? (Expected, normal)
2. Are backends still healthy? (Check Backend Health panel)
3. Did circuit breaker open? (Would explain latency)
4. Is pool hit rate dropping? (Connection overhead)

### Rate Limiter Rejecting Everything
⚠️ Your limit is too low or load is legitimate spike
- Decision: Increase limit? Or accept rejection?
- Watch P99 latency: if still low, rejection is working correctly

### Circuit Breaker Open
🚨 Backend is failing
- Check Prometheus logs: `docker compose logs gogate`
- Verify backend health: `curl http://localhost:8001/status/200`
- Wait for CB to auto-recover (half-open phase)

---

## Useful Prometheus Queries

Open http://localhost:9090 and paste these queries:

```promql
# Current request rate
rate(gogate_http_requests_total[1m])

# P99 latency
histogram_quantile(0.99, rate(gogate_http_request_duration_seconds_bucket[5m]))

# Rate limiting rejections
rate(gogate_rate_limiter_requests_total{status="rejected"}[1m])

# Circuit breaker opens per backend
increase(gogate_circuit_breaker_state{state="open"}[5m])

# Connection pool efficiency
rate(gogate_pool_hits_total[5m]) / (rate(gogate_pool_hits_total[5m]) + rate(gogate_pool_misses_total[5m]))
```

---

## Cleanup

```bash
# Stop all services
docker compose down

# Remove data volumes (reset metrics)
docker compose down -v
```

---

## Next Steps

1. **Customize dashboard**: Edit `deployments/grafana/dashboards/gogate-overview.json`
2. **Set alerting rules**: Add to Prometheus for auto-alerts
3. **Stress test**: Run longer soaks with production-like patterns
4. **Profile**: Use `go tool pprof` to identify bottlenecks
5. **Scale**: Deploy to Kubernetes using Helm chart
