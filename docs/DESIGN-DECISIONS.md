# Design decisions

Why GoGate is shaped the way it is, including the choices that turned out to be
wrong and what replaced them.

## Contents

- [Two layers in one binary](#two-layers-in-one-binary)
- [Three load balancing algorithms](#three-load-balancing-algorithms)
- [The circuit breaker state machine](#the-circuit-breaker-state-machine)
- [Rate limiting: token bucket without a ticker](#rate-limiting-token-bucket-without-a-ticker)
- [Half-close, and the pooled-connection race it caused](#half-close-and-the-pooled-connection-race-it-caused)
- [Connection pooling and its liveness check](#connection-pooling-and-its-liveness-check)
- [Buffer pooling: what it costs](#buffer-pooling-what-it-costs)
- [Non-blocking Start with a Ready channel](#non-blocking-start-with-a-ready-channel)
- [Health checking: active and threshold-based](#health-checking-active-and-threshold-based)
- [Choosing a synchronisation primitive](#choosing-a-synchronisation-primitive)
- [What this does not do](#what-this-does-not-do)

---

## Two layers in one binary

L4 and L7 are not two implementations of the same thing. They are different
problems that happen to share a backend pool.

**L4** (`internal/proxy/tcp.go`) copies bytes. It never parses what it forwards,
so it works for Postgres, Redis, gRPC, TLS passthrough — anything. It cannot
route on a header, authenticate a request, or retry, because it does not know
where one request ends and the next begins. Its unit of work is a *connection*.

**L7** (`internal/proxy/http.go`) parses HTTP, runs a middleware chain, and
re-issues each request to the backend as a new request. Its unit of work is a
*request*, and one client connection may carry thousands of them.

That difference is not cosmetic, and getting it wrong produced the most
memorable debugging session in this project. A k6 run reported 60,000 requests
while the proxy's metrics reported 50. Both were correct. k6 used HTTP
keep-alive, so 50 virtual users held 50 TCP connections and sent thousands of
requests down each. The L4 proxy counted connections, because connections are
what it can see. Nothing was broken; the mental model was.

The two layers share `LoadBalancer`, the backend set and the health checker, but
each keeps **its own circuit breakers**. A backend can complete a TCP handshake
while returning 502s over HTTP — the whole reason `check_type: http` exists — so
letting one layer's failures trip the other's breaker would eject a backend that
is fine for the traffic that layer carries.

## Three load balancing algorithms

Three exist because the right answer depends on what a "request" costs, and that
differs between the two layers.

**Round robin** (`loadbalancer.RoundRobin`) assumes backends are
interchangeable and every request costs the same. It is a counter and a modulo,
with a linear scan forward to skip unhealthy backends. Under L7 with short
uniform requests it is hard to beat.

**Weighted round robin** (`loadbalancer.WeightedRoundRobin`) handles backends of
different capacity, and uses NGINX's *smooth* variant rather than the obvious
one. The naive implementation — emit a backend `weight` times, then move on —
sends weights `[5,1,1]` as `A A A A A B C`: a five-request burst at one backend
while the others idle. The smooth algorithm adds each backend's weight to a
running counter, picks the highest, then subtracts the total, producing
`A A B A A C A`. Same ratio, no bursts. It costs one extra int per backend.

**Least connections** (`loadbalancer.LeastConnections`) scans all backends and
picks the fewest in-flight. This is the right choice for L4, where connections
are long-lived and round robin would be counting the wrong thing entirely — a
backend holding 500 idle connections and one holding 5 look identical to a
counter.

Its known weakness is documented rather than hidden: selection takes a read lock
and does not reserve the chosen backend, so concurrent callers can all pick the
same one. A freshly added backend at zero connections attracts a thundering herd.
Fixing that means an atomic decrement-and-reserve on selection, which serialises
the hot path — a worse trade for the pool sizes this is built for.

## The circuit breaker state machine

```
CLOSED    --[failure_threshold failures within window]--> OPEN
OPEN      --[timeout elapsed]-------------------------->  HALF-OPEN
HALF-OPEN --[success_threshold successes]------------->   CLOSED
HALF-OPEN --[any failure]------------------------------>  OPEN
```

The breaker exists so that a backend everyone already knows is dead stops
absorbing connection timeouts. Failing in microseconds instead of waiting out a
30-second dial keeps the caller's own thread pool from filling with doomed
requests — which is the mechanism by which one dead service takes down the
services in front of it.

**The interesting state is HALF-OPEN, and the first implementation of it was
wrong.** `Allow()` returned `nil` unconditionally in half-open. Sequentially
that looks fine and all the tests passed. Concurrently it is exactly the failure
the breaker exists to prevent: every caller blocked behind an open circuit
reaches `Allow()` within microseconds of the timeout expiring, so the entire
backlog lands on a backend that was just declared dead. The state was a label,
not admission control.

Half-open is now bounded. `MaxHalfOpenRequests` caps probes in flight; everyone
else keeps getting `ErrCircuitOpen` until a probe reports back. Slots are
released in `RecordResult` and zeroed on every state transition, so an abandoned
probe cannot wedge the breaker shut.

`MaxHalfOpenRequests` defaults to `SuccessThreshold` rather than to 1. With
`SuccessThreshold: 3` and a cap of 1, a recovered backend needs three sequential
round trips to close the circuit; with a cap of 3 it needs one. Both bound the
load the same way — the cap is what the backend feels — but the default
recovers three times faster. Set it to 1 when probing is genuinely expensive.

Thresholds are asymmetric on purpose. Five failures to open, one success to
close: opening on a fluke is cheap to recover from, staying open when the
backend is healthy is not.

The window matters as much as the threshold. Counting failures without one means
five failures spread over a week eventually trip the breaker; the rolling window
(default 60s) makes the threshold mean "five failures *now*".

**What the L4 breaker actually measures** is worth being precise about. On the
TCP path, `RecordResult(nil)` fires as soon as the backend connection is
established, because after that the proxy is copying opaque bytes and has no
notion of a failed request. So the L4 breaker tracks *connect* failures, not
request failures. The L7 breaker sees round-trip errors and is the more
meaningful of the two.

## Rate limiting: token bucket without a ticker

Token bucket over sliding window because bursts are a feature. Real traffic
arrives in clumps, and a limiter that refuses a 20-request burst against a
1000/s limit is rejecting traffic the backend could trivially absorb. `burst`
sets how far above the sustained rate a spike may go; `rate` sets what it
converges to.

The implementation detail that matters: **there is no refill goroutine.**
`refill()` computes tokens from `time.Since(lastFill)` on each call. The obvious
alternative — a `time.Ticker` adding tokens — needs a goroutine per limiter and
a lifecycle to stop it, and every limiter that outlives its stop path leaks that
goroutine forever. Lazy refill has no lifecycle to get wrong.

Both listeners share one bucket, so the configured rate is a budget for the
process rather than per-layer.

The limitation is inherent: this is per-process. Three replicas configured at
1000/s admit 3000/s. Distributed limiting needs shared state (Redis, or a
token-lease protocol), and pretending otherwise would be worse than saying so.

## Half-close, and the pooled-connection race it caused

TCP lets each direction close independently. A client that has finished sending
sends FIN but must keep reading — that is how a request/response protocol
signals "request complete, response please". A proxy that responds to FIN by
calling `Close()` tears down both directions and truncates the response.

So each copy direction ends with `CloseWrite()` on its destination, forwarding
the FIN one way and leaving the other open. `net.TCPConn` has `CloseWrite`, but
the proxy holds a `net.Conn`, and the pooled wrapper embeds rather than
re-exports it — hence the duck-typed
`dst.(interface{ CloseWrite() error })` in `copyWithHalfCloseAndCount`.

**Combining half-close with connection pooling produced a real race.**
`pooledConn` tracked two facts as independent atomics: has this been returned to
the pool, and has it sent FIN. `CloseWrite` read `returned` (false), and before
it could store `halfClosed`, a concurrent `Close()` completed its own
check-and-set, saw `halfClosed` still false, and put the connection back in the
idle pool. The next request to check that connection out inherited a socket
about to receive FIN: a truncated response, with no error anywhere to explain it.

The pooling decision depends on both facts, so both now live under one mutex.
These are once-per-connection operations, not per-byte, so the lock is nowhere
near the copy path.

The window is a few instructions wide. `TestPooledConnConcurrentCloseAndCloseWrite`
runs 50,000 trials, counts every violation of the invariant — half-closed implies
not pooled — and reports the total rather than stopping at the first one. It
asserts the invariant rather than any particular interleaving, so it stays
meaningful if the scheduling changes.

Run against the previous implementation on this machine:

| | violations / 50,000 trials |
|---|---|
| `go test` | 1, 2, 2, 2, 3, 1 (six runs) |
| `go test -race` | 214, 165, 211 (three runs) |

Counting rather than aborting on the first violation is what makes that table
possible; the first version of this test called `t.Fatalf` inside the loop and
could only ever report "1".

The bug is there in both columns — this is not a race the detector invents. What
`-race` changes is the odds: without it the invariant breaks roughly once per
20,000 attempts, which in production is an occasional truncated response that
looks like a network flake and never reproduces on demand. That ratio is the
argument for `-race` in CI in one line.

## Connection pooling and its liveness check

Reusing a backend connection skips a handshake. It also risks handing out a
connection the backend closed while it sat idle, turning a saved round trip into
a failed request.

`pooledConn.usable()` checks before every reuse: set a zero read deadline,
attempt a one-byte read, and classify.

- **Timeout** — nothing to read, the peer has not closed. Reuse it.
- **EOF or `net.ErrClosed`** — the backend closed. Drop it.
- **Data available** — drop it. Unread bytes mean the previous user left the
  protocol mid-stream, and consuming them here would corrupt whoever gets it
  next. Cheaper to dial than to guess.

The pool itself is a buffered channel rather than a slice with a mutex.
`select` with `default` gives non-blocking get (empty → dial a new one) and
non-blocking put (full → close it), which is the entire policy, with no lock to
hold across a `Close()`.

Connections carry both an idle timeout and a maximum lifetime. Idle timeout
catches the peer that quietly went away; max lifetime bounds how long a
connection can outlive things like a backend rollout or a DNS change that the
proxy has no other way to notice.

## Buffer pooling: what it costs

Each copy direction needs a buffer. Allocating 32 KB per copy produces enormous
garbage, so buffers come from a `sync.Pool` (as `*[]byte` — pooling a bare
`[]byte` allocates on every `Get` for the interface conversion, which
staticcheck SA6002 flags).

The win is real: **32,840 B/op down to 72 B/op** in the copy benchmark, and zero
allocations per operation on the steady-state proxy path.

The cost is real too, and is not visible in a benchmark. Each connection runs two
copy goroutines, each holding a 32 KB buffer, and `io.CopyBuffer` blocks inside
the copy loop for the connection's entire lifetime — so neither buffer returns to
the pool until the connection closes. A heap profile at 3,000 concurrent
connections attributes **96.75% of live heap** to that one `sync.Pool` New
function: 64,048 bytes per connection, exactly two 32 KB buffers.

Buffer pooling trades allocation churn for pinned memory proportional to
concurrency. For a gateway fronting many mostly-idle long-lived connections that
is the wrong trade, and shrinking the buffer is the highest-leverage change
available — 4 KB buffers would cut per-connection memory roughly 8x at the cost
of more read syscalls on busy connections. See
[BENCHMARKS.md](BENCHMARKS.md#where-the-130-kb-goes).

## Non-blocking Start with a Ready channel

`Start()` returns immediately and exposes `Ready()`, closed once the accept loop
is running. It began as a blocking call, which pushed goroutine management onto
every caller and made tests racy:

```go
// Before: the sleep is a guess, and CI eventually calls the bluff
go proxy.Start(ctx)
time.Sleep(50 * time.Millisecond)

// After
proxy.Start(ctx)
<-proxy.Ready()
```

The accept loop cannot simply block on `Accept()`, because then it cannot notice
context cancellation. It sets a one-second deadline before each `Accept`, treats
the resulting timeout as normal, and re-checks the context. Shutdown latency is
bounded by that deadline.

A proxy is **not restartable after `Stop()`**. `ready` and `done` are closed
channels afterwards, and re-arming them means either recreating them (racing
every caller holding the old one) or a generation counter. Constructing a new
proxy costs nothing and removes the entire class of bug.

**Where graceful shutdown is honest about its limits:** `Stop()` closes the
listener, waits for the accept loop, then waits on a `WaitGroup` of live
connections until the context deadline. It does not force-close connections that
are still open. An idle-but-open L4 connection — a pooled database connection,
say — never returns from its copy loop, so `Stop()` burns its full deadline and
returns `shutdown timeout: N connections still active`. In Kubernetes the
subsequent SIGKILL handles it. Closing tracked connections after the grace
period would be the correct fix and is not implemented.

## Health checking: active and threshold-based

Passive health checking — infer health from request failures — reacts fast but
only learns about backends that are receiving traffic. A backend the load
balancer has already ejected generates no failures, so nothing ever tells you it
recovered. Active probes run on their own interval regardless of traffic, which
is what makes recovery possible.

Transitions require *consecutive* results: `unhealthy_threshold` failures to
eject, `healthy_threshold` successes to reinstate. Flipping on a single probe
means one dropped packet ejects a healthy backend, and with several backends
flapping independently the pool composition changes faster than anything
downstream can adapt.

`check_type: tcp` proves only that the port is open — the kernel completes the
handshake on behalf of a process that has stopped serving, so a deadlocked
backend passes. `check_type: http` requires a 2xx from `http_path`. It requires
2xx rather than "any response" because a backend returning 503 from its health
endpoint is telling you it is not ready, and reading that as healthy defeats the
point of asking. Redirects are not followed: a redirecting health endpoint is
misconfigured, and following it could silently probe a different host.

## Choosing a synchronisation primitive

Four primitives are used, and the choice is by access pattern rather than habit:

**Atomics** for standalone counters — `activeConns`, `totalConns`, request
counts. Read on the hot path, never read together with anything else, so there
is no invariant a lock would protect.

**`RWMutex`** for read-heavy structures — the backend list, the registry, the
route table. Many concurrent readers, occasional writer.

**`Mutex`** where every access mutates. `WeightedRoundRobin.Next()` updates
running weights on every call, so an `RWMutex` would be strictly worse: same
exclusion, more bookkeeping. Same for the token bucket, whose `Allow()` always
consumes.

**Channels** for signalling — `ready`, `done`, registry watch events, the idle
connection pool. Channels are for handing off ownership; mutexes are for
protecting shared state. Using a channel to guard a counter is how you get code
that is both slower and harder to read.

Two patterns recur:

*Double-checked locking* for lazily created map entries (per-backend circuit
breakers, per-backend metrics): read-lock and look, and only if absent take the
write lock and look again. Without the second check two goroutines both create
the entry and one silently discards the other's — which for a circuit breaker
means losing failure counts.

*Atomic under a read lock* in `RoundRobin.Next()`: `mu.RLock()` guards the slice
against concurrent replacement while `counter.Add(1)` advances the cursor. The
read lock is not protecting the counter — the atomic does that — it is
protecting the slice the counter indexes into.

## What this does not do

Stated plainly, because a gateway that quietly does not do these things is worse
than one that says so:

- **TLS termination.** Backends are dialled over plain HTTP (`cloneRequest`
  hardcodes the scheme).
- **Distributed rate limiting.** Per-process only; N replicas admit N times the
  configured rate.
- **WebSocket and gRPC over L7.** Neither survives the request/response model in
  `ServeHTTP`. Both work through the L4 listener, which does not care.
- **Retries.** A failed backend request returns 502; it is not re-issued to
  another backend.
- **Authentication in the default chain.** The JWT validator in `internal/auth`
  is complete and tested, but nothing in the config schema carries a signing key
  yet, so the L7 chain does not install it. A gateway that appears to
  authenticate and does not is worse than one that does not claim to.
- **Distributed tracing.** Request IDs propagate; there are no spans and no
  OpenTelemetry export.
- **Persistent service registry.** In-memory with TTL; registrations do not
  survive a restart.
- **Forced connection close on shutdown.** See
  [Non-blocking Start](#non-blocking-start-with-a-ready-channel).
