# Measurements

Every number here was produced by a command in this repository, and the command
is printed next to it. Where a measurement is unreliable, that is stated rather
than rounded away.

## Environment

| | |
|---|---|
| Machine | Apple M1 Pro, 8 cores, macOS 26.5.2 |
| Go | 1.26.2 darwin/arm64 |
| Topology | client, proxy and backend in one process over loopback |
| Load average during runs | 21-30 (the machine was **not** idle) |

The load average matters. Absolute latency figures below carry several hundred
percent of run-to-run variance and should be read as "same order of magnitude",
not as a specification. The allocation counts and the capacity ceiling are not
affected by machine load and are reliable.

## Concurrent connection capacity

```
GOGATE_CAPACITY=8000 go test ./internal/proxy -run ConnectionCapacity -v -timeout 15m
GOGATE_CAPACITY=8000 GOGATE_CAPACITY_DIRECT=1 \
    go test ./internal/proxy -run ConnectionCapacity -v -timeout 15m
```

Every connection is held open simultaneously and every one completes a write and
a read through the proxy before the measurement is taken. A socket that was
accepted but never served does not count.

| | proxied | direct (no proxy) |
|---|---|---|
| Connections established | 8,000 / 8,000 | 8,000 / 8,000 |
| Failures | 0 | 0 |
| Establish rate | 2,961 conn/s | 10,355 conn/s |
| Goroutines | +4.0 per connection | +1.0 per connection |
| Heap in use | 1,058.6 MB (135.5 KB/conn) | 40.5 MB (5.2 KB/conn) |
| Goroutine stacks | 147.8 MB (18.9 KB/conn) | 18.2 MB (2.3 KB/conn) |
| Process memory (Sys) | 1,253 MB | 91.5 MB |

The `direct` column runs the identical client load straight against the echo
backend with the proxy removed. Subtracting it isolates the proxy's own cost,
because client, proxy and backend all share one process and one heap:

**Per proxied connection: ~130 KB heap, ~17 KB goroutine stack, 3 goroutines.**

### Where the 130 KB goes

```
GOGATE_CAPACITY=3000 go test ./internal/proxy -run ConnectionCapacity \
    -memprofile cap.mem -memprofilerate=1
go tool pprof -inuse_space -top -nodecount=12 cap.mem
```

```
Showing nodes accounting for 198226.77kB, 99.81% of 198603.34kB total
      flat  flat%   sum%        cum   cum%
192145.44kB 96.75% 96.75% 192145.44kB 96.75%  gogate/internal/proxy.NewTCPProxy.func1
 6081.16kB  3.06% 99.81%  6081.16kB  3.06%  runtime.mallocgc
    0.17kB    ~   99.81% 192304.48kB 96.83%  ...(*TCPProxy).copyWithHalfCloseAndCount
         0     0% 99.81% 192151.41kB 96.75%  sync.(*Pool).Get
```

`NewTCPProxy.func1` is the `sync.Pool` New function that allocates a 32 KB copy
buffer. 192,145 kB across 3,000 connections is 64,048 bytes each: exactly two
32 KB buffers per connection.

That is the whole story. Each connection runs two copy goroutines
(client→backend, backend→client), each holds a pooled 32 KB buffer, and
`io.CopyBuffer` blocks inside the copy loop for the connection's entire
lifetime — so neither buffer is returned to the pool until the connection
closes. Buffer pooling trades allocation churn for pinned memory proportional to
concurrency, and at high connection counts the pinned memory is the dominant
cost. Nothing else in the proxy accounts for more than 3%.

The remaining ~66 KB between the profile's exact 64 KB and the 135 KB MemStats
figure is GC headroom: `HeapAlloc` is measured after a forced GC but the
allocator still holds spans it has not returned.

For a gateway fronting many mostly-idle long-lived connections, a smaller copy
buffer is the single highest-leverage change available. 4 KB buffers would cut
per-connection memory by roughly 8x at the cost of more read syscalls on
high-throughput connections.

### The measured ceiling is loopback ports, not the proxy

At 10,000 the harness fails:

```
requested:    10000
established:  8166
failed:       1834
first error:  dial tcp 127.0.0.1:56781: connect: can't assign requested address
```

macOS ephemeral range is 49152-65535, which is 16,384 ports. Each proxied
connection consumes two of them (client→proxy and proxy→backend both originate
on loopback). 8,166 × 2 = 16,332. The proxy was not the limit; the test topology
was. Reaching five figures requires the backend on a different host, or a widened
`net.inet.ip.portrange`.

**So: 8,000 concurrent connections is measured. 10,000 is not, and is not
claimed.**

## Copy path

```
go test ./internal/proxy -bench BenchmarkCopyBuffer -benchmem -benchtime=2s -count=3
```

| | ns/op | throughput | B/op | allocs/op |
|---|---|---|---|---|
| `CopyBufferWithPool` | 7,318 - 9,789 | 6.7 - 9.0 GB/s | 72 | 3 |
| `CopyBufferNoPool` | 27,990 - 74,438 | 0.9 - 2.3 GB/s | 32,840 | 4 |

64 KB payload, 32 KB buffer, so roughly two buffer fills per operation.

**Allocation is the reliable figure: 32,840 B/op down to 72 B/op.** The latency
ratio moves with machine load; the allocation count does not.

### This benchmark used to measure nothing

It previously copied a `bytes.Reader` into `io.Discard` and reported
**448,052 MB/s**. `bytes.Reader` implements `io.WriterTo` and `io.Discard`
implements `io.ReaderFrom`, so `io.CopyBuffer` took a shortcut and never touched
the buffer it was handed — it was timing the allocation of an unused 32 KB slice.
Nothing memcpys at 448 GB/s, which is what gave it away.

The fix wraps both ends in types that hide those interfaces.
`TestCopyBufferBenchmarkUsesTheBuffer` poisons the buffer and fails if the copy
leaves it untouched, so the benchmark cannot silently revert to measuring
allocation again.

## End-to-end proxy latency

```
go test ./internal/proxy -bench BenchmarkTCPProxy -benchmem -benchtime=2s -count=5
```

| | range over 5 runs |
|---|---|
| `TCPProxyConcurrency` | 52,100 - 82,931 ns/op, 0 allocs/op |
| `TCPProxyConcurrencyWithTCPPool` | 59,637 - 77,897 ns/op, 0 allocs/op |
| `TCPProxyThroughput` | 74,771 - 377,062 ns/op, 0 allocs/op |

**These numbers are not trustworthy on this machine and no headline latency is
claimed from them.** A single client doing request/response ping-pong through a
proxy measures the wakeup latency of four goroutines across two loopback sockets;
on a box with a load average of 21+ that is dominated by scheduler queueing. The
same benchmark produced 74,771 ns/op and 377,062 ns/op in two runs an hour apart.

Two things can still be read off them:

- **Zero allocations per operation on the steady-state proxy path.** This is
  stable across every run and is the property the buffer pool exists to provide.
- **The TCP connection pool shows no measurable latency effect here.** The two
  concurrency ranges overlap almost entirely. That is expected rather than
  disappointing: the pool removes backend `connect()` calls, and this benchmark
  holds connections open, so there are almost none to remove. Its value shows up
  in workloads that open and close backend connections, which is what
  `BenchmarkBackendDial{NoPool,WithPool}` isolates.

An honest end-to-end latency number needs the load generator on a separate host
from the proxy, and an idle proxy host. The `k6/` scripts are set up for that;
the numbers are not in this repo because that measurement has not been run.

## Reproducing

```bash
# Correctness, including the concurrency regression tests
go test -race ./...

# Copy path allocations
go test ./internal/proxy -bench BenchmarkCopyBuffer -benchmem -benchtime=2s -count=3

# Connection capacity (opens 4 file descriptors per connection)
GOGATE_CAPACITY=8000 go test ./internal/proxy -run ConnectionCapacity -v -timeout 15m

# Same load without the proxy, for the baseline to subtract
GOGATE_CAPACITY=8000 GOGATE_CAPACITY_DIRECT=1 \
    go test ./internal/proxy -run ConnectionCapacity -v -timeout 15m
```

Leave 60 seconds between capacity runs. Sockets from the previous run sit in
`TIME_WAIT` for 2×MSL and will exhaust the ephemeral port range if you do not.
