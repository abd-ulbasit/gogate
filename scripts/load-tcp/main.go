package main

import (
	"context"
	"flag"
	"io"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

// Lightweight TCP load generator for GoGate's TCP proxy.
// Example: go run ./scripts/load-tcp -addr localhost:8081 -concurrency 200 -rate 500 -duration 30s

func main() {
	addr := flag.String("addr", "localhost:8080", "TCP address of GoGate TCP listener")
	payload := flag.String("payload", "hello", "payload to echo")
	concurrency := flag.Int("concurrency", 50, "number of worker goroutines")
	rate := flag.Int("rate", 200, "requests per second total")
	duration := flag.Duration("duration", 30*time.Second, "test duration")
	flag.Parse()

	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	var success uint64
	var failure uint64
	wg := sync.WaitGroup{}
	work := make(chan struct{}, *concurrency)

	for i := 0; i < *concurrency; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range work {
				conn, err := net.DialTimeout("tcp", *addr, 2*time.Second)
				if err != nil {
					atomic.AddUint64(&failure, 1)
					continue
				}
				conn.SetDeadline(time.Now().Add(5 * time.Second))
				if _, err := conn.Write([]byte(*payload)); err != nil {
					atomic.AddUint64(&failure, 1)
					_ = conn.Close()
					continue
				}
				// Signal we're done writing so proxy/server knows to flush response
				if tcpConn, ok := conn.(*net.TCPConn); ok {
					_ = tcpConn.CloseWrite()
				}
				buf := make([]byte, len(*payload))
				if _, err := io.ReadFull(conn, buf); err != nil {
					atomic.AddUint64(&failure, 1)
				} else {
					atomic.AddUint64(&success, 1)
				}
				_ = conn.Close()
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
			logger.Info("tcp load test complete", "success", success, "failure", failure)
			return
		case <-stop.C:
			close(work)
			wg.Wait()
			logger.Info("tcp load test complete", "success", success, "failure", failure)
			return
		case <-ticker.C:
			select {
			case work <- struct{}{}:
			default:
			}
		}
	}
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}
