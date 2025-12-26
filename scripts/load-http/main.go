package main

import (
	"context"
	"flag"
	"log/slog"
	"math"
	"net/http"
	"os"
	"os/signal"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

// Lightweight HTTP load generator for GoGate.
// Example: go run ./scripts/load-http -target http://localhost:8080/echo -concurrency 50 -rate 200 -duration 30s

func main() {
	target := flag.String("target", "http://localhost:8080/", "target URL (proxy endpoint)")
	method := flag.String("method", "GET", "HTTP method")
	body := flag.String("body", "", "request body (optional)")
	concurrency := flag.Int("concurrency", 20, "number of worker goroutines")
	rate := flag.Int("rate", 200, "requests per second total")
	duration := flag.Duration("duration", 30*time.Second, "test duration")
	timeout := flag.Duration("timeout", 5*time.Second, "per-request timeout")
	flag.Parse()

	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	client := &http.Client{Timeout: *timeout}

	var success uint64
	var failure uint64
	latencies := make([]time.Duration, 0, *rate*int(duration.Seconds())/10)
	latMu := sync.Mutex{}

	work := make(chan struct{}, *concurrency)
	wg := sync.WaitGroup{}

	for i := 0; i < *concurrency; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range work {
				start := time.Now()
				req, _ := http.NewRequestWithContext(ctx, *method, *target, strings.NewReader(*body))
				resp, err := client.Do(req)
				if err != nil {
					atomic.AddUint64(&failure, 1)
				} else {
					_ = resp.Body.Close()
					if resp.StatusCode >= 200 && resp.StatusCode < 300 {
						atomic.AddUint64(&success, 1)
					} else {
						atomic.AddUint64(&failure, 1)
					}
				}
				latency := time.Since(start)
				latMu.Lock()
				latencies = append(latencies, latency)
				latMu.Unlock()
			}
		}()
	}

	ticker := time.NewTicker(time.Second / time.Duration(max(1, *rate)))
	stop := time.NewTimer(*duration)

	for {
		select {
		case <-ctx.Done():
			close(work)
			wg.Wait()
			printSummary(logger, success, failure, latencies)
			return
		case <-stop.C:
			close(work)
			wg.Wait()
			printSummary(logger, success, failure, latencies)
			return
		case <-ticker.C:
			select {
			case work <- struct{}{}:
			default:
			}
		}
	}
}

func printSummary(logger *slog.Logger, success, failure uint64, latencies []time.Duration) {
	total := success + failure
	avg := time.Duration(0)
	if len(latencies) > 0 {
		var sum time.Duration
		for _, l := range latencies {
			sum += l
		}
		avg = sum / time.Duration(len(latencies))
	}
	p95 := percentile(latencies, 0.95)
	p99 := percentile(latencies, 0.99)

	logger.Info("load test complete", "total", total, "success", success, "failure", failure, "avg", avg, "p95", p95, "p99", p99)
}

func percentile(latencies []time.Duration, p float64) time.Duration {
	if len(latencies) == 0 {
		return 0
	}
	sorted := make([]time.Duration, len(latencies))
	copy(sorted, latencies)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	idx := int(math.Ceil(p*float64(len(sorted)))) - 1
	if idx < 0 {
		idx = 0
	}
	if idx >= len(sorted) {
		idx = len(sorted) - 1
	}
	return sorted[idx]
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}
