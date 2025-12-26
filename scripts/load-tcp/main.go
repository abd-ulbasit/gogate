package main

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

// TCP-level load generator that sends raw HTTP requests over TCP.
// This tests GoGate at the TCP layer while still being compatible with HTTP backends.
// Example: go run ./scripts/load-tcp -addr localhost:8080 -concurrency 50 -rate 200 -duration 30s

func main() {
	addr := flag.String("addr", "localhost:8080", "TCP address of GoGate TCP listener")
	path := flag.String("path", "/get", "HTTP path to request")
	concurrency := flag.Int("concurrency", 50, "number of worker goroutines")
	rate := flag.Int("rate", 200, "requests per second total")
	duration := flag.Duration("duration", 30*time.Second, "test duration")
	keepalive := flag.Bool("keepalive", false, "reuse TCP connections (HTTP keep-alive)")
	flag.Parse()

	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	var success, failure, totalBytes uint64
	wg := sync.WaitGroup{}
	work := make(chan struct{}, *concurrency)

	// Build the HTTP request payload
	httpReq := buildHTTPRequest(*path, *keepalive)
	logger.Info("starting TCP load test",
		"addr", *addr,
		"path", *path,
		"concurrency", *concurrency,
		"rate", *rate,
		"duration", *duration,
		"keepalive", *keepalive,
	)

	for i := 0; i < *concurrency; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()

			var conn net.Conn
			var reader *bufio.Reader

			for range work {
				// If no connection or not using keepalive, create new connection
				if conn == nil {
					var err error
					conn, err = net.DialTimeout("tcp", *addr, 2*time.Second)
					if err != nil {
						atomic.AddUint64(&failure, 1)
						continue
					}
					reader = bufio.NewReader(conn)
				}

				conn.SetDeadline(time.Now().Add(5 * time.Second))

				// Send raw HTTP request over TCP
				if _, err := conn.Write(httpReq); err != nil {
					atomic.AddUint64(&failure, 1)
					conn.Close()
					conn = nil
					continue
				}

				// Read HTTP response
				resp, err := http.ReadResponse(reader, nil)
				if err != nil {
					atomic.AddUint64(&failure, 1)
					conn.Close()
					conn = nil
					continue
				}

				// Drain and count response body
				buf := make([]byte, 4096)
				for {
					n, err := resp.Body.Read(buf)
					atomic.AddUint64(&totalBytes, uint64(n))
					if err != nil {
						break
					}
				}
				resp.Body.Close()

				if resp.StatusCode >= 200 && resp.StatusCode < 400 {
					atomic.AddUint64(&success, 1)
				} else {
					atomic.AddUint64(&failure, 1)
				}

				// Close connection if not using keepalive
				if !*keepalive {
					conn.Close()
					conn = nil
				}
			}

			// Cleanup
			if conn != nil {
				conn.Close()
			}
		}()
	}

	ticker := time.NewTicker(time.Second / time.Duration(max(1, *rate)))
	stop := time.NewTimer(*duration)
	start := time.Now()

	// Progress reporting
	go func() {
		progressTicker := time.NewTicker(5 * time.Second)
		defer progressTicker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-progressTicker.C:
				s := atomic.LoadUint64(&success)
				f := atomic.LoadUint64(&failure)
				elapsed := time.Since(start).Seconds()
				logger.Info("progress",
					"success", s,
					"failure", f,
					"req/s", fmt.Sprintf("%.1f", float64(s+f)/elapsed),
				)
			}
		}
	}()

	for {
		select {
		case <-ctx.Done():
			close(work)
			wg.Wait()
			printResults(logger, success, failure, totalBytes, time.Since(start))
			return
		case <-stop.C:
			close(work)
			wg.Wait()
			printResults(logger, success, failure, totalBytes, time.Since(start))
			return
		case <-ticker.C:
			select {
			case work <- struct{}{}:
			default:
			}
		}
	}
}

func buildHTTPRequest(path string, keepalive bool) []byte {
	connHeader := "close"
	if keepalive {
		connHeader = "keep-alive"
	}
	return []byte(fmt.Sprintf(
		"GET %s HTTP/1.1\r\nHost: localhost\r\nConnection: %s\r\nUser-Agent: load-tcp/1.0\r\n\r\n",
		path, connHeader,
	))
}

func printResults(logger *slog.Logger, success, failure, totalBytes uint64, elapsed time.Duration) {
	total := success + failure
	rate := float64(total) / elapsed.Seconds()
	successRate := float64(success) / float64(max(1, int(total))) * 100
	throughput := float64(totalBytes) / elapsed.Seconds() / 1024 / 1024

	logger.Info("TCP load test complete",
		"success", success,
		"failure", failure,
		"total", total,
		"duration", elapsed.Round(time.Millisecond),
		"req/s", fmt.Sprintf("%.1f", rate),
		"success_rate", fmt.Sprintf("%.1f%%", successRate),
		"throughput_MB/s", fmt.Sprintf("%.2f", throughput),
	)
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}
