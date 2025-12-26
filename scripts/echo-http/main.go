package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync/atomic"
	"syscall"
	"time"
)

// Simple HTTP echo server that self-registers with the GoGate registry and sends periodic heartbeats.
// Run multiple instances on different ports to act as demo backends.

func main() {
	listen := flag.String("listen", ":9001", "listen address for the echo server")
	host := flag.String("host", "localhost", "public host used for registry address")
	name := flag.String("name", "echo-http", "service name for registry")
	tagsCSV := flag.String("tags", "http,echo", "comma-separated tags")
	weight := flag.Int("weight", 1, "backend weight")
	ttl := flag.Duration("ttl", 30*time.Second, "service TTL for registry")
	registryURL := flag.String("registry", "http://localhost:8500", "base URL for registry (no trailing slash)")
	noRegistry := flag.Bool("no-registry", true, "if set, skip registry registration/heartbeat (default true since registry is optional)")
	flag.Parse()

	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	_, addrPort, err := net.SplitHostPort(*listen)
	if err != nil {
		logger.Error("invalid listen address", "listen", *listen, "error", err)
		os.Exit(1)
	}
	address := net.JoinHostPort(*host, addrPort)

	mux := http.NewServeMux()
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("OK"))
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		resp := map[string]any{
			"method":  r.Method,
			"path":    r.URL.Path,
			"headers": r.Header,
			"body":    string(body),
			"time":    time.Now().Format(time.RFC3339Nano),
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	})

	server := &http.Server{Addr: *listen, Handler: mux}

	go func() {
		logger.Info("http echo server starting", "addr", *listen)
		if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			logger.Error("http server error", "error", err)
			cancel()
		}
	}()

	var svcID string
	if !*noRegistry {
		svcID = registerHTTP(ctx, logger, *registryURL, *name, address, parseTagsHTTP(*tagsCSV), *weight, *ttl)
		if svcID != "" {
			go heartbeatHTTP(ctx, logger, *registryURL, svcID, *ttl)
		}
	}

	<-ctx.Done()
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer shutdownCancel()
	logger.Info("http echo server stopping")
	_ = server.Shutdown(shutdownCtx)
}

func parseTagsHTTP(csv string) []string {
	if csv == "" {
		return nil
	}
	parts := strings.Split(csv, ",")
	tags := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			tags = append(tags, p)
		}
	}
	return tags
}

func registerHTTP(ctx context.Context, logger *slog.Logger, baseURL, name, address string, tags []string, weight int, ttl time.Duration) string {
	registerURL := strings.TrimSuffix(baseURL, "/") + "/v1/services"
	payload := map[string]any{
		"name":    name,
		"address": address,
		"tags":    tags,
		"weight":  weight,
		"ttl":     ttl.String(),
	}

	attempts := int32(0)
	for {
		attempts++
		buf, _ := json.Marshal(payload)
		req, _ := http.NewRequestWithContext(ctx, http.MethodPost, registerURL, bytes.NewReader(buf))
		req.Header.Set("Content-Type", "application/json")

		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			logger.Warn("registry register failed", "error", err, "attempt", atomic.LoadInt32(&attempts))
		} else {
			defer resp.Body.Close()
			if resp.StatusCode >= 200 && resp.StatusCode < 300 {
				var regResp struct {
					ID string `json:"id"`
				}
				_ = json.NewDecoder(resp.Body).Decode(&regResp)
				id := regResp.ID
				if id == "" {
					logger.Warn("registry did not return id; continuing with empty id")
				} else {
					logger.Info("registered with registry", "id", id, "address", address, "ttl", ttl)
					return id
				}
			} else {
				body, _ := io.ReadAll(resp.Body)
				logger.Warn("registry rejected registration", "status", resp.StatusCode, "body", string(body), "attempt", atomic.LoadInt32(&attempts))
			}
		}

		select {
		case <-ctx.Done():
			return ""
		case <-time.After(2 * time.Second):
		}
	}
}

func heartbeatHTTP(ctx context.Context, logger *slog.Logger, baseURL, id string, ttl time.Duration) {
	if id == "" {
		return
	}
	interval := ttl / 2
	if interval <= 0 {
		interval = 10 * time.Second
	}

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	url := strings.TrimSuffix(baseURL, "/") + "/v1/services/" + id + "/heartbeat"
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			req, _ := http.NewRequestWithContext(ctx, http.MethodPut, url, nil)
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				logger.Warn("heartbeat failed", "error", err)
				continue
			}
			resp.Body.Close()
			if resp.StatusCode >= 200 && resp.StatusCode < 300 {
				logger.Debug("heartbeat ok")
			} else {
				logger.Warn("heartbeat rejected", "status", resp.StatusCode)
			}
		}
	}
}
