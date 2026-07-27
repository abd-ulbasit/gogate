<h1 align="center">Sluice</h1>

<p align="center">
  <strong>An L4/L7 proxy and API gateway in Go, with no dependencies in the request path.</strong>
</p>

<p align="center">
  <a href="https://github.com/abd-ulbasit/sluice/actions"><img src="https://github.com/abd-ulbasit/sluice/workflows/CI/badge.svg" alt="CI Status"></a>
  <a href="LICENSE"><img src="https://img.shields.io/badge/License-MIT-blue.svg" alt="License"></a>
</p>

Sluice runs two listeners over one backend pool. The L4 listener copies raw TCP
bytes, so it fronts Postgres, Redis or anything else that is not HTTP. The L7
listener parses HTTP, runs a middleware chain, and re-issues each request to the
backend. They share a load balancer, health checker and metrics; they differ in
how far up the stack a request is inspected — and, as it turns out, in almost
everything that follows from that.

Two parts are worth reading the code for.

---

## The half-open state was a label, not admission control

A three-state circuit breaker is a standard exercise. The state machine is four
transitions and the tests pass on the first try:

```
CLOSED    --[failure_threshold failures in window]--> OPEN
OPEN      --[timeout elapsed]---------------------->  HALF-OPEN
HALF-OPEN --[success_threshold successes]-------->    CLOSED
HALF-OPEN --[any failure]------------------------->   OPEN
```

The first implementation here returned `nil` unconditionally from `Allow()` in
HALF-OPEN, and every sequential test agreed it was correct.

Concurrently it does the opposite of its job. Every caller blocked behind an open
circuit reaches `Allow()` within microseconds of the timeout expiring, so the
first thing a backend sees after being declared dead is the entire accumulated
backlog at once — the stampede the breaker exists to prevent. The bug is
invisible to any test that calls `Allow()` in a loop.

Half-open is now bounded. `MaxHalfOpenRequests` caps probes in flight; everyone
else keeps getting `ErrCircuitOpen` until a probe reports a result. It defaults
to `SuccessThreshold` rather than 1, so a recovered backend closes the circuit in
one round trip instead of `SuccessThreshold` sequential ones — the cap is what
the backend feels, and both settings bound it identically.

The regression test releases 64 goroutines at a single instant and asserts
exactly `MaxHalfOpenRequests` get through. Against the previous implementation
it reports 64.

> [`internal/circuitbreaker/circuitbreaker.go`](internal/circuitbreaker/circuitbreaker.go)
> · [test](internal/circuitbreaker/circuitbreaker_test.go)
> · [rationale](docs/DESIGN-DECISIONS.md#the-circuit-breaker-state-machine)

## Half-close plus connection pooling is a race

TCP lets each direction close independently, and a proxy has to forward that: a
client sending FIN means "request finished, response please", so the proxy calls
`CloseWrite()` on the backend rather than `Close()`. Get this wrong and responses
truncate.

Combine it with a connection pool and there is a second, narrower problem.
`pooledConn` tracked "returned to the pool" and "sent FIN" as two independent
atomics. `CloseWrite` read `returned` (false); before it could store
`halfClosed`, a concurrent `Close()` finished its own check-and-set, saw
`halfClosed` still false, and returned the connection to the idle pool. The next
request to check it out inherited a socket about to receive FIN — a truncated
response with no error anywhere to explain it.

The pooling decision depends on both facts, so both now live under one mutex.
They are once-per-connection operations, nowhere near the byte-copy path.

The window is a few instructions wide.
`TestPooledConnConcurrentCloseAndCloseWrite` runs 50,000 trials, counts every
violation of the invariant — half-closed implies not pooled — and reports the
total. It asserts the invariant rather than any particular interleaving.

Run against the previous implementation, the count is the argument for `-race`
in CI:

| | violations / 50,000 trials |
|---|---|
| `go test` | 1 - 3 (six runs) |
| `go test -race` | 165 - 214 (three runs) |

The bug is present in both columns; the detector widens the window by roughly
two orders of magnitude. Without it, a real race shows up once per 20,000
attempts and looks like a flake.

> [`internal/backend/backend.go`](internal/backend/backend.go)
> · [test](internal/backend/backend_test.go)
> · [rationale](docs/DESIGN-DECISIONS.md#half-close-and-the-pooled-connection-race-it-caused)

---

## Measurements

Full methodology, commands and caveats in [docs/BENCHMARKS.md](docs/BENCHMARKS.md).

| | measured |
|---|---|
| Concurrent TCP connections | **8,000**, all completing a round trip, 0 failures |
| Cost per proxied connection | ~130 KB heap, ~17 KB stack, 3 goroutines |
| Copy path allocation | **32,840 B/op → 72 B/op** with buffer pooling |
| Steady-state proxy path | 0 allocs/op |

Three things these numbers do **not** say:

**The ceiling is not the proxy.** At 10,000 the harness fails with
`can't assign requested address` after 8,166 connections. macOS has 16,384
ephemeral ports and each proxied connection on loopback burns two of them;
8,166 × 2 = 16,332. Reaching five figures needs the backend on another host. So
8,000 is measured and 10,000 is not claimed.

**Buffer pooling costs memory.** A heap profile at 3,000 connections attributes
**96.75% of live heap** to one `sync.Pool` New function: two 32 KB copy buffers
per connection, pinned for the connection's whole lifetime because
`io.CopyBuffer` blocks inside the copy loop. Pooling trades allocation churn for
memory proportional to concurrency. For many idle long-lived connections, a
smaller buffer is the highest-leverage change available.

**No end-to-end latency figure is published.** The loopback ping-pong benchmark
returned 74,771 ns/op and 377,062 ns/op on two runs of the same code an hour
apart, on a machine at load average 21+. An honest number needs the load
generator on a separate host, which has not been run. The allocation counts above
are load-independent and stand.

<details>
<summary>A benchmark that was measuring nothing at all</summary>

`BenchmarkCopyBufferWithPool` reported **448,052 MB/s** — roughly 7x what the
corrected benchmark measures for a cache-resident copy on this machine, and past
its DRAM bandwidth. The number was the bug report.

It copied a `bytes.Reader` into `io.Discard`. `bytes.Reader` implements
`io.WriterTo` and `io.Discard` implements `io.ReaderFrom`, so `io.CopyBuffer`
took a shortcut and never touched the buffer it was handed — the benchmark was
timing the allocation of an unused 32 KB slice.

Both ends are now wrapped in types that hide those interfaces, and
`TestCopyBufferBenchmarkUsesTheBuffer` poisons the buffer and fails if the copy
leaves it untouched.

The corrected benchmark's own ns/op moves 6-7x with machine load, so no latency
figure from it is quoted here. Its allocation counts are identical across every
run, which is why the table above quotes those.
[docs/BENCHMARKS.md](docs/BENCHMARKS.md#copy-path) prints both load states.
</details>

---

## Quick start

```bash
go build -o bin/sluice ./cmd/sluice
cp config.example.yaml config.yaml   # edit backends
./bin/sluice -config config.yaml
```

With the demo backends from `scripts/`:

```bash
go run ./scripts/echo-http -listen :9001 -registry "" &
go run ./scripts/echo-http -listen :9002 -registry "" &
./bin/sluice -config config.yaml
```

```bash
curl http://localhost:8081/          # L7: middleware chain, X-Request-ID, metrics
curl http://localhost:9090/health    # admin
curl http://localhost:9090/metrics   # prometheus
```

The L4 listener in front of those same HTTP backends demonstrates what raw
passthrough means:

```console
$ printf 'hello\n' | nc localhost 8080
HTTP/1.1 400 Bad Request
```

That 400 is the correct result and comes from the backend, not the proxy. L4
forwarded `hello\n` verbatim; an HTTP server received it and rejected it as
malformed. Point the L4 listener at `scripts/echo-tcp` (or Postgres, or Redis)
and the same code path carries that protocol untouched — it never parses what it
forwards.

Watching the breaker trip and recover — kill one backend and drive traffic:

```
502 200 502 200 503 200 503 200 503 200
 │       │       └── circuit open: failing fast, no dial attempted
 │       └────────── healthy backend, unaffected
 └────────────────── real backend failure, counted toward the threshold
```

Restart the backend, wait out `circuit_breaker.timeout`, and
`sluice_circuit_breaker_state` returns to `0`.

### Docker

```bash
docker compose -f deployments/docker/docker-compose.yaml up -d
```

Brings up the proxy, three backends, Prometheus and Grafana.

### Kubernetes

```bash
helm install sluice ./deployments/helm/sluice \
  --set 'backends[0].addr=my-service:8080' \
  --namespace sluice --create-namespace
```

---

## What it does

**Traffic management**

| | |
|---|---|
| Load balancing | Round robin, NGINX smooth weighted round robin, least connections |
| Health checks | Active TCP or HTTP probes, consecutive-result thresholds to stop flapping |
| Circuit breaking | Per backend, per layer, three states with bounded half-open probing |
| Rate limiting | Token bucket, lazy refill (no ticker goroutine). One process-wide bucket — a backend budget, with no per-client dimension |
| Traffic splitting | Weighted, for canary and A/B |
| Service discovery | TTL registration with heartbeat; expiry removes the backend from the pool |

**L4 (TCP)** — raw byte copy, correct half-close, connection pooling with a
liveness check before reuse, connection tracking for least-connections and
draining.

**L7 (HTTP)** — reverse proxy with `Recovery → Tracing → Metrics → RateLimit →
Headers`, host and path-prefix routing, hop-by-hop header stripping, tuned
`http.Transport`, `X-Request-ID` propagation.

**Operations** — hot config reload via fsnotify, graceful shutdown with
connection draining, Prometheus metrics, structured JSON logs, Helm chart with
HPA/PDB/ServiceMonitor.

## Configuration

[`config.example.yaml`](config.example.yaml) is the full reference. Every key in
it is read by [`internal/config`](internal/config/config.go); settings the binary
does not implement are not listed.

```yaml
server:
  listen: ":8080"        # L4
  http_listen: ":8081"   # L7, omit to run L4 only

  backends:
    - addr: "backend-1:8080"
      weight: 5
    - addr: "backend-canary:8080"
      weight: 1

  load_balancer: "weighted_round_robin"

  health:
    interval: 5s
    check_type: "http"          # "tcp" only proves the port is open
    http_path: "/health"
    unhealthy_threshold: 3
    healthy_threshold: 2

  circuit_breaker:
    enabled: true
    failure_threshold: 5
    success_threshold: 2
    max_half_open_requests: 2   # probes admitted at once when recovering
    timeout: 30s
    window: 60s

  rate_limit:
    enabled: true
    rate: 1000
    burst: 100
```

`internal/config` tests parse every shipped config and the Helm template and
assert the features are really enabled — yaml.v3 ignores unknown keys, so a
misspelled optional key disables a feature with no error and no log line.

## Observability

`:9090/metrics`, Prometheus text format, generated without `client_golang`:

```
sluice_backend_health{backend="10.0.0.5:8080"} 1
sluice_circuit_breaker_state{backend="10.0.0.5:8080"} 0   # 0=closed 1=open 2=half-open
sluice_rate_limiter_requests_total{result="rejected"} 0
sluice_pool_hits_total{backend="10.0.0.5:8080"} 0
sluice_pool_misses_total{backend="10.0.0.5:8080"} 0
sluice_request_duration_seconds_bucket{le="0.05"} 19204
```

### The L4 pool hit counter stays at zero, by construction

Not a misconfiguration. Each copy direction ends by half-closing its
destination (`internal/proxy/tcp.go`, the `CloseWrite` after the copy loop),
and a half-closed connection is never returned to the pool. That second half is
asserted by `TestPooledConnHalfClosedIsNotPooled` in
`internal/backend/backend_test.go`. Put together, every L4 request dials a
fresh backend socket, so the hit counter cannot move on that path.

Making it move means not forwarding the client's FIN to the backend. That was
tried and it is worse than an idle counter: a client that half-closes and then
waits receives an empty response, and because the abandoned socket is returned
to the pool with a reply still owed on it, the next client can be served the
previous client's response body. Both were reproduced before the change was
reverted. Connection reuse is not worth a protocol-visible correctness bug, so
the pool remains useful on the L7 path and the L4 counter stays honest.

The attempt is on
[`attempted/tcp-pool-reuse`](https://github.com/abd-ulbasit/sluice/tree/attempted/tcp-pool-reuse),
one commit off `main`, kept so the claim above can be checked rather than taken.

Admin API: `GET /health`, `GET /stats`, `GET /backends`.

## Architecture

```
                 ┌──────────────────────── Sluice ────────────────────────┐
                 │                                                        │
  raw TCP ──────▶│  L4 listener ──────────────────────────┐               │
                 │  byte copy, half-close, conn pool      │               │
                 │                                        ▼               │
                 │  L7 listener ──▶ Recovery ─▶ Tracing ─▶ Load balancer  │──▶ backends
  HTTP ─────────▶│  parse, re-issue   Metrics ─▶ RateLimit  RR/WRR/LC     │
                 │                    Headers            ▲                │
                 │                                       │                │
                 │  Health checker ──────────────────────┤                │
                 │  Circuit breakers (per backend, ──────┘                │
                 │   per layer)                                           │
                 └────────────────────────────────────────────────────────┘
```

Both listeners share the backend set, load balancer and health checker. Circuit
breakers are per layer: a backend can accept TCP while returning 502s over HTTP,
so one layer's failures must not eject it from the other.

## Development

```bash
go test -race ./...          # what CI runs; the pooled-connection race needs -race to show reliably
go test ./... -bench=. -benchmem
SLUICE_CAPACITY=8000 go test ./internal/proxy -run ConnectionCapacity -v -timeout 15m
```

```
cmd/sluice/           entry point, listener and middleware wiring
internal/
  proxy/              L4 tcp.go, L7 http.go, capacity harness
  backend/            backend abstraction, connection pool, half-close
  circuitbreaker/     three-state breaker with bounded half-open
  loadbalancer/       RR, smooth WRR, least connections
  health/             active TCP and HTTP probes
  ratelimiter/        token bucket
  middleware/         L7 chain
  registry/           TTL service discovery
  router/ splitter/ auth/ metrics/ observability/ config/
deployments/          Docker Compose, Helm chart, Grafana dashboards
k6/                   load test scripts
docs/                 design decisions, measurements
```

## Limitations

No TLS termination (backends are dialled over plain HTTP). Rate limiting is one
process-wide bucket: it caps what the backend pool is asked to absorb, but it
has no per-client dimension, so one caller at the limit starves the rest and the
proxy cannot tell that from legitimate load. It is also per-process, so N
replicas admit N times the configured rate. WebSocket and gRPC
work through L4 but not L7. Failed backend requests return 502 and are not
retried. The JWT validator is complete and tested but not installed in the
default middleware chain, because the config schema carries no signing key yet —
a gateway that appears to authenticate and does not is worse than one that makes
no claim. `Stop()` drains in-flight connections but cannot force-close an idle
L4 connection, so it burns its full grace period before returning.

Each of these, and why, is in
[docs/DESIGN-DECISIONS.md](docs/DESIGN-DECISIONS.md#what-this-does-not-do).

## How this was built

Most of the commits here carry a `Co-authored-by: Claude` trailer — `git log
--grep='^Co-authored-by: Claude' -i --oneline | wc -l` against `git rev-list
--count HEAD` if you want the ratio. I build with coding agents and review,
benchmark and integrate what comes back. The parts that decided the shape of
this project were not generated, and they are what the sections above are
about: the half-open breaker returned `nil` unconditionally and passed every
sequential test it was given, so finding it meant reading the state machine and
asking what N callers arriving in the same microsecond would do;
`BenchmarkCopyBufferWithPool` was caught because 448,052 MB/s is past this
machine's DRAM bandwidth, not because anything failed; and the L4 hit counter
stays at zero because the change that would have moved it was written,
reproduced against a real client, and reverted — that branch is still pushed so
the revert is inspectable rather than asserted. If you want to judge the
engineering rather than the tooling, read
[docs/BENCHMARKS.md](docs/BENCHMARKS.md), which still prints the numbers that
contradicted the claims they were meant to support, and
[docs/DESIGN-DECISIONS.md](docs/DESIGN-DECISIONS.md).

## Further reading

- [docs/DESIGN-DECISIONS.md](docs/DESIGN-DECISIONS.md) — why the code is shaped
  this way, including the choices that were wrong
- [docs/BENCHMARKS.md](docs/BENCHMARKS.md) — measurements, methodology, and what
  the numbers do not support
- [LOAD_TESTING.md](LOAD_TESTING.md) — k6 scenarios and dashboard interpretation

## License

MIT — see [LICENSE](LICENSE).
