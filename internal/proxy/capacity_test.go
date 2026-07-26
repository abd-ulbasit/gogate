package proxy

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"runtime"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// Concurrent-connection capacity harness.
//
// This exists because "handles N concurrent connections" is the kind of claim
// that gets written into a README without anything behind it. It measures three
// things that can be checked rather than asserted:
//
//   - how many simultaneous TCP connections the proxy actually holds open with
//     every one of them completing a round trip,
//   - what those connections cost in heap,
//   - how much of that cost is the proxy rather than the test harness.
//
// The third point is the reason for the `direct` mode. Client, proxy and echo
// backend all run in one process, so a raw heap figure measures the harness as
// much as the proxy. Running the identical client load straight against the echo
// server, with the proxy removed, gives a baseline to subtract. What remains is
// the proxy's own per-connection cost: its accepted socket, its dialled socket,
// and the three goroutines it runs per connection.
//
// Skipped by default - it opens tens of thousands of file descriptors and is not
// something CI should do on every push.
//
//	SLUICE_CAPACITY=10000 go test ./internal/proxy -run ConnectionCapacity -v -timeout 20m
//	SLUICE_CAPACITY=10000 SLUICE_CAPACITY_DIRECT=1 \
//	    go test ./internal/proxy -run ConnectionCapacity -v -timeout 20m
//
// Every logical connection costs 4 descriptors in proxied mode (client socket,
// proxy's accepted socket, proxy's backend socket, backend's accepted socket)
// and 2 in direct mode, so raise the file descriptor limit before running at
// five figures.
func TestTCPProxyConnectionCapacity(t *testing.T) {
	target := os.Getenv("SLUICE_CAPACITY")
	if target == "" {
		t.Skip("set SLUICE_CAPACITY=<n> to run the connection capacity harness")
	}
	n, err := strconv.Atoi(target)
	if err != nil || n <= 0 {
		t.Fatalf("SLUICE_CAPACITY must be a positive integer, got %q", target)
	}
	direct := os.Getenv("SLUICE_CAPACITY_DIRECT") != ""

	echo := mustStartEchoServer()
	defer echo.Close()

	dialTarget := echo.Addr().String()
	if !direct {
		lb := buildRoundRobin(echo)
		logger := slog.New(slog.NewTextHandler(io.Discard, nil))
		p := NewTCPProxy("127.0.0.1:0", lb, logger)

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		if err := p.Start(ctx); err != nil {
			t.Fatalf("start proxy: %v", err)
		}
		<-p.Ready()
		defer p.Stop(context.Background())
		dialTarget = p.Addr()
	}

	// Baseline after everything is running but before any client connects.
	runtime.GC()
	var before runtime.MemStats
	runtime.ReadMemStats(&before)
	goroutinesBefore := runtime.NumGoroutine()

	var (
		conns     = make([]net.Conn, n)
		connected atomic.Int64
		failed    atomic.Int64
		errMu     sync.Mutex
		firstErr  error
		wg        sync.WaitGroup
	)

	// Dial in bounded parallelism: a 10k-wide burst measures the kernel's accept
	// backlog, not the proxy.
	const dialConcurrency = 256
	sem := make(chan struct{}, dialConcurrency)

	start := time.Now()
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			c, err := net.DialTimeout("tcp", dialTarget, 10*time.Second)
			if err != nil {
				failed.Add(1)
				recordErr(&errMu, &firstErr, err)
				return
			}

			// Prove the connection is actually usable end to end, not just
			// accepted. An accepted-but-never-served socket is not capacity.
			msg := []byte(fmt.Sprintf("ping-%d\n", idx))
			buf := make([]byte, len(msg))
			c.SetDeadline(time.Now().Add(30 * time.Second))
			if _, err := c.Write(msg); err != nil {
				failed.Add(1)
				recordErr(&errMu, &firstErr, err)
				c.Close()
				return
			}
			if _, err := io.ReadFull(c, buf); err != nil {
				failed.Add(1)
				recordErr(&errMu, &firstErr, err)
				c.Close()
				return
			}
			c.SetDeadline(time.Time{})

			conns[idx] = c
			connected.Add(1)
		}(i)
	}
	wg.Wait()
	establishDuration := time.Since(start)

	// Measure with every connection still open.
	runtime.GC()
	var after runtime.MemStats
	runtime.ReadMemStats(&after)
	goroutinesAfter := runtime.NumGoroutine()

	established := connected.Load()
	heapDelta := int64(after.HeapAlloc) - int64(before.HeapAlloc)

	mode := "proxied"
	if direct {
		mode = "direct (no proxy)"
	}

	t.Logf("mode:                  %s", mode)
	t.Logf("requested:             %d", n)
	t.Logf("established:           %d", established)
	t.Logf("failed:                %d", failed.Load())
	errMu.Lock()
	if firstErr != nil {
		t.Logf("first error:           %v", firstErr)
	}
	errMu.Unlock()
	t.Logf("establish duration:    %s (%.0f conn/s)",
		establishDuration.Round(time.Millisecond),
		float64(established)/establishDuration.Seconds())
	t.Logf("goroutines:            %d -> %d (+%d, %.1f per connection)",
		goroutinesBefore, goroutinesAfter, goroutinesAfter-goroutinesBefore,
		float64(goroutinesAfter-goroutinesBefore)/float64(max64(established, 1)))
	t.Logf("heap in use:           %.1f MB -> %.1f MB (delta %.1f MB)",
		mb(before.HeapAlloc), mb(after.HeapAlloc), float64(heapDelta)/(1024*1024))
	t.Logf("heap per connection:   %.1f KB", float64(heapDelta)/float64(max64(established, 1))/1024)
	// Goroutine stacks are reported separately from the heap, so a per-connection
	// figure taken from HeapAlloc alone silently omits them.
	stackDelta := int64(after.StackInuse) - int64(before.StackInuse)
	t.Logf("goroutine stacks:      %.1f MB delta (%.1f KB per connection)",
		float64(stackDelta)/(1024*1024), float64(stackDelta)/float64(max64(established, 1))/1024)
	t.Logf("process memory (Sys):  %.1f MB -> %.1f MB", mb(before.Sys), mb(after.Sys))
	t.Logf("total heap allocated:  %.1f MB (cumulative, includes freed)", mb(after.TotalAlloc-before.TotalAlloc))

	for _, c := range conns {
		if c != nil {
			c.Close()
		}
	}

	if failed.Load() > 0 {
		t.Errorf("%d of %d connections failed; capacity claims must cite the number that succeeded",
			failed.Load(), n)
	}
}

func mb(bytes uint64) float64 { return float64(bytes) / (1024 * 1024) }

func max64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}

// recordErr keeps the first error seen. atomic.Value cannot be used here: it
// panics on CompareAndSwap when the stored values have different concrete types,
// and the dial/read paths return different error types.
func recordErr(mu *sync.Mutex, dst *error, err error) {
	mu.Lock()
	if *dst == nil {
		*dst = err
	}
	mu.Unlock()
}
