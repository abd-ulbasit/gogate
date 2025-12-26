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

// Simple TCP echo server that self-registers with the GoGate registry and sends periodic heartbeats.
// Each connection echoes back what it reads.

func main() {
	listen := flag.String("listen", ":9001", "listen address for the TCP echo server")
	host := flag.String("host", "localhost", "public host used for registry address")
	name := flag.String("name", "echo-tcp", "service name for registry")
	tagsCSV := flag.String("tags", "tcp,echo", "comma-separated tags")
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

	ln, err := net.Listen("tcp", *listen)
	if err != nil {
		logger.Error("listen failed", "error", err)
		os.Exit(1)
	}
	logger.Info("tcp echo server listening", "addr", *listen)

	var svcID string
	if !*noRegistry {
		svcID = registerTCP(ctx, logger, *registryURL, *name, address, parseTagsTCP(*tagsCSV), *weight, *ttl)
		if svcID != "" {
			go heartbeatTCP(ctx, logger, *registryURL, svcID, *ttl)
		}
	}

	go func() {
		<-ctx.Done()
		_ = ln.Close()
	}()

	for {
		conn, err := ln.Accept()
		if err != nil {
			select {
			case <-ctx.Done():
				logger.Info("tcp echo server stopping")
				return
			default:
			}
			logger.Warn("accept error", "error", err)
			continue
		}
		go handleConn(logger, conn)
	}
}

func handleConn(logger *slog.Logger, conn net.Conn) {
	defer conn.Close()

	// Use io.Copy for a cleaner echo - it handles buffering properly
	// and returns when the client closes their write side (EOF)
	n, err := io.Copy(conn, conn)
	if err != nil && err != io.EOF {
		logger.Warn("echo error", "error", err, "bytes", n)
	}

	// Half-close our write side to signal we're done writing
	// This sends FIN to the client/proxy cleanly
	if tcpConn, ok := conn.(*net.TCPConn); ok {
		_ = tcpConn.CloseWrite()
	}
}

func parseTagsTCP(csv string) []string {
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

func registerTCP(ctx context.Context, logger *slog.Logger, baseURL, name, address string, tags []string, weight int, ttl time.Duration) string {
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

func heartbeatTCP(ctx context.Context, logger *slog.Logger, baseURL, id string, ttl time.Duration) {
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
