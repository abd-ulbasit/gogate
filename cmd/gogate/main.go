package main

import (
	"context"
	"encoding/json"
	"flag"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"time"

	"gogate/internal/backend"
	"gogate/internal/circuitbreaker"
	"gogate/internal/config"
	"gogate/internal/health"
	"gogate/internal/loadbalancer"
	"gogate/internal/metrics"
	"gogate/internal/observability"
	"gogate/internal/proxy"
	"gogate/internal/ratelimiter"
	"gogate/internal/registry"

	"github.com/fsnotify/fsnotify"
)

func main() {
	// Parse command-line flags
	configPath := flag.String("config", "config.yaml", "path to config file")
	flag.Parse()

	// Load configuration
	cfg, err := config.Load(*configPath)
	if err != nil {
		slog.Error("failed to load config", "error", err)
		os.Exit(1)
	}

	// Validate configuration
	if err := cfg.Validate(); err != nil {
		slog.Error("invalid config", "error", err)
		os.Exit(1)
	}

	// Set up structured logging from config
	logger := observability.NewLogger(observability.LogConfig{
		Level:     cfg.Server.Logging.Level,
		Format:    cfg.Server.Logging.Format,
		Output:    os.Stdout,
		AddSource: cfg.Server.Logging.AddSource,
	})

	logger.Info("starting gogate",
		"listen", cfg.Server.ListenAddr,
		"backends", len(cfg.Server.Backends),
		"lb", cfg.Server.LoadBalancer,
		"rate_limit", cfg.Server.RateLimit.Enabled,
		"circuit_breaker", cfg.Server.CircuitBreaker.Enabled,
		"admin", cfg.Server.Admin.Enabled,
		"registry", cfg.Server.Registry.Enabled,
	)

	// Build backend objects
	backends := make([]*backend.Backend, len(cfg.Server.Backends))
	for i, b := range cfg.Server.Backends {
		if b.Weight > 1 {
			backends[i] = backend.NewBackendWithWeight(b.Addr, b.Weight)
		} else {
			backends[i] = backend.NewBackend(b.Addr)
		}

		// Apply TCP pooling config (opt-in)
		if cfg.Server.TCPPool.Enabled {
			poolCfg := backend.DefaultPoolConfig()
			if cfg.Server.TCPPool.MaxIdle > 0 {
				poolCfg.MaxIdle = cfg.Server.TCPPool.MaxIdle
			}
			if cfg.Server.TCPPool.IdleTimeout > 0 {
				poolCfg.IdleTimeout = cfg.Server.TCPPool.IdleTimeout
			}
			if cfg.Server.TCPPool.MaxLifetime > 0 {
				poolCfg.MaxLifetime = cfg.Server.TCPPool.MaxLifetime
			}
			backends[i].EnablePooling(&poolCfg)
		} else {
			backends[i].EnablePooling(nil)
		}
	}

	// Choose load balancer
	var lb loadbalancer.LoadBalancer
	switch cfg.Server.LoadBalancer {
	case "least_connections":
		lb = loadbalancer.NewLeastConnections(backends)
	case "weighted_round_robin":
		lb = loadbalancer.NewWeightedRoundRobin(backends)
	default:
		lb = loadbalancer.NewRoundRobin(backends)
	}

	// Start health checker
	checkerCfg := health.CheckerConfig{
		Interval:           cfg.Server.Health.Interval,
		Timeout:            cfg.Server.Health.Timeout,
		UnhealthyThreshold: cfg.Server.Health.UnhealthyThreshold,
		HealthyThreshold:   cfg.Server.Health.HealthyThreshold,
		CheckType:          cfg.Server.Health.CheckType,
		HTTPPath:           cfg.Server.Health.HTTPPath,
	}
	checker := health.NewChecker(backends, logger, checkerCfg)

	// Build proxy options
	var proxyOpts []proxy.Option

	// Metrics collector (always enabled for admin endpoint)
	metricsCollector := metrics.NewCollector()
	proxyOpts = append(proxyOpts, proxy.WithMetrics(metricsCollector))

	// Rate limiter (optional)
	if cfg.Server.RateLimit.Enabled {
		rl := ratelimiter.NewTokenBucket(cfg.Server.RateLimit.Rate, cfg.Server.RateLimit.Burst)
		proxyOpts = append(proxyOpts, proxy.WithRateLimiter(rl))
		logger.Info("rate limiter enabled",
			"rate", cfg.Server.RateLimit.Rate,
			"burst", cfg.Server.RateLimit.Burst,
		)
	}

	// Circuit breaker (optional)
	if cfg.Server.CircuitBreaker.Enabled {
		cbConfig := circuitbreaker.Config{
			FailureThreshold: cfg.Server.CircuitBreaker.FailureThreshold,
			SuccessThreshold: cfg.Server.CircuitBreaker.SuccessThreshold,
			Timeout:          cfg.Server.CircuitBreaker.Timeout,
			Window:           cfg.Server.CircuitBreaker.Window,
		}
		proxyOpts = append(proxyOpts, proxy.WithCircuitBreaker(cbConfig))
		logger.Info("circuit breaker enabled",
			"failure_threshold", cbConfig.FailureThreshold,
			"timeout", cbConfig.Timeout,
		)
	}

	// Create TCP proxy with options
	tcpProxy := proxy.NewTCPProxy(
		cfg.Server.ListenAddr,
		lb,
		logger,
		proxyOpts...,
	)

	// Set up graceful shutdown
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Signal handler: SIGINT (Ctrl+C), SIGTERM (systemd, k8s)
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, os.Interrupt)
	// Also handle SIGTERM on Unix platforms without importing syscall explicitly
	// Note: On non-Unix platforms, SIGTERM may not be present; Interrupt is sufficient

	// Config reload state
	var configMu sync.RWMutex
	currentConfig := cfg

	// Handle signals in goroutine (shutdown only)
	go func() {
		for range sigChan {
			logger.Info("received shutdown signal")
			cancel()
			return
		}
	}()

	// Start config file watcher for hot reload
	reloadBackends := func(newCfg *config.Config) {
		configMu.Lock()
		oldConfig := currentConfig
		currentConfig = newCfg
		configMu.Unlock()

		if backendsChanged(oldConfig, newCfg) {
			logger.Info("backends changed, updating load balancer")
			newBackends := buildBackends(newCfg, logger)
			lb.UpdateBackends(newBackends)
			checker.UpdateBackends(newBackends)
		}

		logger.Info("config reloaded successfully")
	}

	if err := startConfigWatcher(ctx, *configPath, logger, reloadBackends); err != nil {
		logger.Warn("config watcher not started", "error", err)
	}

	// Create Prometheus exporter with metric sources
	promExporter := metrics.NewPrometheusExporter(metricsCollector)

	// Set up backend health getter for Prometheus
	promExporter.SetBackendHealthGetter(func() map[string]bool {
		result := make(map[string]bool)
		for _, b := range lb.Backends() {
			result[b.Addr()] = b.IsHealthy()
		}
		return result
	})

	// Set up pool stats getter for Prometheus
	promExporter.SetPoolStatsGetter(func() map[string]metrics.PoolStatsSnapshot {
		result := make(map[string]metrics.PoolStatsSnapshot)
		for _, b := range lb.Backends() {
			stats := b.PoolStats()
			result[b.Addr()] = metrics.PoolStatsSnapshot{
				Hits:     stats.Hits,
				Misses:   stats.Misses,
				Returned: stats.Returned,
				Dropped:  stats.Dropped,
				Size:     stats.Size,
			}
		}
		return result
	})

	// Start admin server (optional)
	var adminServer *http.Server
	if cfg.Server.Admin.Enabled {
		adminDeps := AdminServerDeps{
			Collector:    metricsCollector,
			LoadBalancer: lb,
			Exporter:     promExporter,
			Logger:       logger,
		}
		adminServer = startAdminServer(cfg.Server.Admin.Listen, adminDeps)
		logger.Info("prometheus metrics available", "endpoint", "http://"+cfg.Server.Admin.Listen+"/metrics")
	}

	// Start registry server (optional)
	var registryServer *http.Server
	var serviceRegistry *registry.Registry
	if cfg.Server.Registry.Enabled {
		regConfig := registry.Config{
			DefaultTTL:      cfg.Server.Registry.DefaultTTL,
			CleanupInterval: cfg.Server.Registry.CleanupInterval,
			MaxServices:     cfg.Server.Registry.MaxServices,
		}
		serviceRegistry = registry.New(regConfig, logger)
		registryServer = startRegistryServer(cfg.Server.Registry.Listen, serviceRegistry, lb, checker, logger)

		// Subscribe to registry changes to update load balancer
		go watchRegistryChanges(serviceRegistry, lb, checker, cfg, logger)
	}

	// Start health checker (non-blocking)
	if err := checker.Start(ctx); err != nil {
		logger.Error("failed to start health checker", "error", err)
		os.Exit(1)
	}

	// Start proxy (non-blocking - returns immediately)
	if err := tcpProxy.Start(ctx); err != nil {
		logger.Error("failed to start proxy", "error", err)
		os.Exit(1)
	}

	// Wait until proxy is ready to accept connections
	<-tcpProxy.Ready()
	logger.Info("proxy ready, accepting connections")

	// Wait for context cancellation (signal received)
	<-ctx.Done()
	logger.Info("shutting down...")

	// Graceful shutdown with timeout
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer shutdownCancel()

	// Stop admin server
	if adminServer != nil {
		if err := adminServer.Shutdown(shutdownCtx); err != nil {
			logger.Warn("admin server shutdown error", "error", err)
		}
	}

	// Stop registry server
	if registryServer != nil {
		if err := registryServer.Shutdown(shutdownCtx); err != nil {
			logger.Warn("registry server shutdown error", "error", err)
		}
	}
	if serviceRegistry != nil {
		serviceRegistry.Stop()
	}

	if err := tcpProxy.Stop(shutdownCtx); err != nil {
		logger.Error("shutdown error", "error", err)
		os.Exit(1)
	}
	checker.Stop()

	logger.Info("shutdown complete")
}

// AdminServerDeps holds dependencies for the admin server.
type AdminServerDeps struct {
	Collector    *metrics.Collector
	LoadBalancer loadbalancer.LoadBalancer
	Exporter     *metrics.PrometheusExporter
	Logger       *slog.Logger
}

// startAdminServer starts the admin HTTP server for metrics and stats.
func startAdminServer(addr string, deps AdminServerDeps) *http.Server {
	mux := http.NewServeMux()

	// GET /stats - returns metrics snapshot as JSON
	mux.HandleFunc("GET /stats", func(w http.ResponseWriter, r *http.Request) {
		snapshot := deps.Collector.Snapshot()
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(snapshot)
	})

	// GET /backends - returns backend health status
	mux.HandleFunc("GET /backends", func(w http.ResponseWriter, r *http.Request) {
		backends := deps.LoadBalancer.Backends()
		type backendStatus struct {
			Addr              string `json:"addr"`
			Weight            int    `json:"weight"`
			Healthy           bool   `json:"healthy"`
			ActiveConnections int64  `json:"active_connections"`
		}
		status := make([]backendStatus, len(backends))
		for i, b := range backends {
			status[i] = backendStatus{
				Addr:              b.Addr(),
				Weight:            b.Weight(),
				Healthy:           b.IsHealthy(),
				ActiveConnections: b.ActiveConnections(),
			}
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(status)
	})

	// GET /health - simple health check for the proxy itself
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("OK"))
	})

	// GET /metrics - Prometheus metrics endpoint
	if deps.Exporter != nil {
		mux.HandleFunc("GET /metrics", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
			if err := deps.Exporter.Export(w); err != nil {
				deps.Logger.Error("failed to export metrics", "error", err)
				http.Error(w, "failed to export metrics", http.StatusInternalServerError)
			}
		})
	}

	server := &http.Server{
		Addr:    addr,
		Handler: mux,
	}

	go func() {
		deps.Logger.Info("admin server started", "addr", addr)
		if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			deps.Logger.Error("admin server error", "error", err)
		}
	}()

	return server
}

// startRegistryServer starts the service registry HTTP server.
func startRegistryServer(addr string, reg *registry.Registry, lb loadbalancer.LoadBalancer, checker *health.Checker, logger *slog.Logger) *http.Server {
	handler := registry.NewHandler(reg, logger)

	mux := http.NewServeMux()
	mux.Handle("/v1/services", handler)
	mux.Handle("/v1/services/", handler)

	server := &http.Server{
		Addr:    addr,
		Handler: mux,
	}

	go func() {
		logger.Info("registry server started", "addr", addr)
		if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			logger.Error("registry server error", "error", err)
		}
	}()

	return server
}

// watchRegistryChanges listens for registry events and updates the load balancer.
func watchRegistryChanges(reg *registry.Registry, lb loadbalancer.LoadBalancer, checker *health.Checker, cfg *config.Config, logger *slog.Logger) {
	events := reg.Subscribe()
	defer reg.Unsubscribe(events)

	for event := range events {
		logger.Info("registry event",
			"type", event.Type.String(),
			"service_id", event.Service.ID,
			"service_name", event.Service.Name,
			"address", event.Service.Address,
		)

		// Rebuild backends from registry + config
		allBackends := buildBackendsFromRegistry(reg, cfg, logger)
		lb.UpdateBackends(allBackends)
		checker.UpdateBackends(allBackends)
	}
}

// startConfigWatcher watches the config file for changes and triggers hot reloads.
// It debounces rapid successive events to avoid duplicate reloads on a single save.
func startConfigWatcher(ctx context.Context, configPath string, logger *slog.Logger, onReload func(*config.Config)) error {
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		return err
	}

	// We watch the directory because many editors write via atomic rename
	// Derive directory and filename
	dir := "."
	file := configPath
	if idx := lastSlash(configPath); idx >= 0 {
		dir = configPath[:idx]
		file = configPath[idx+1:]
		if dir == "" {
			dir = "."
		}
	}

	if err := watcher.Add(dir); err != nil {
		_ = watcher.Close()
		return err
	}

	// Debounce timer
	const debounce = 200 * time.Millisecond
	var (
		timer *time.Timer
		mu    sync.Mutex
	)

	trigger := func() {
		mu.Lock()
		defer mu.Unlock()
		if timer != nil {
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
		}
		timer = time.AfterFunc(debounce, func() {
			// Load config
			newCfg, err := config.Load(configPath)
			if err != nil {
				logger.Error("failed to reload config", "error", err)
				return
			}
			if err := newCfg.Validate(); err != nil {
				logger.Error("invalid config on reload", "error", err)
				return
			}
			onReload(newCfg)
		})
	}

	go func() {
		logger.Info("config watcher started", "path", configPath)
		defer func() {
			_ = watcher.Close()
			logger.Info("config watcher stopped")
		}()
		for {
			select {
			case <-ctx.Done():
				return
			case ev, ok := <-watcher.Events:
				if !ok {
					return
				}
				// Only react to the configured file
				if !eventTargetsFile(ev, file) {
					continue
				}
				// Reload on write, create, remove, rename, chmod
				if ev.Op&(fsnotify.Write|fsnotify.Create|fsnotify.Remove|fsnotify.Rename|fsnotify.Chmod) != 0 {
					trigger()
				}
			case err, ok := <-watcher.Errors:
				if !ok {
					return
				}
				logger.Warn("config watcher error", "error", err)
			}
		}
	}()

	return nil
}

// eventTargetsFile returns true if the fsnotify event concerns the given filename.
func eventTargetsFile(ev fsnotify.Event, filename string) bool {
	// Path may include directory; compare by suffix match on path separator + filename or exact match
	if ev.Name == filename {
		return true
	}
	// naive suffix check
	if len(ev.Name) >= len(filename) && ev.Name[len(ev.Name)-len(filename):] == filename {
		return true
	}
	return false
}

// lastSlash returns the index of the last path separator ('/' or '\\') in s, or -1 if none.
func lastSlash(s string) int {
	last := -1
	for i := 0; i < len(s); i++ {
		if s[i] == '/' || s[i] == '\\' {
			last = i
		}
	}
	return last
}

// buildBackends creates backend objects from config.
func buildBackends(cfg *config.Config, logger *slog.Logger) []*backend.Backend {
	backends := make([]*backend.Backend, len(cfg.Server.Backends))
	for i, b := range cfg.Server.Backends {
		if b.Weight > 1 {
			backends[i] = backend.NewBackendWithWeight(b.Addr, b.Weight)
		} else {
			backends[i] = backend.NewBackend(b.Addr)
		}

		// Apply TCP pooling config (opt-in)
		if cfg.Server.TCPPool.Enabled {
			poolCfg := backend.DefaultPoolConfig()
			if cfg.Server.TCPPool.MaxIdle > 0 {
				poolCfg.MaxIdle = cfg.Server.TCPPool.MaxIdle
			}
			if cfg.Server.TCPPool.IdleTimeout > 0 {
				poolCfg.IdleTimeout = cfg.Server.TCPPool.IdleTimeout
			}
			if cfg.Server.TCPPool.MaxLifetime > 0 {
				poolCfg.MaxLifetime = cfg.Server.TCPPool.MaxLifetime
			}
			backends[i].EnablePooling(&poolCfg)
		} else {
			backends[i].EnablePooling(nil)
		}
	}
	return backends
}

// buildBackendsFromRegistry combines config backends with registry services.
func buildBackendsFromRegistry(reg *registry.Registry, cfg *config.Config, logger *slog.Logger) []*backend.Backend {
	// Start with config backends
	backends := buildBackends(cfg, logger)

	// Add registered services
	for _, svc := range reg.List() {
		b := backend.NewBackendWithWeight(svc.Address, svc.Weight)

		// Apply TCP pooling if enabled in config
		if cfg.Server.TCPPool.Enabled {
			poolCfg := backend.DefaultPoolConfig()
			if cfg.Server.TCPPool.MaxIdle > 0 {
				poolCfg.MaxIdle = cfg.Server.TCPPool.MaxIdle
			}
			if cfg.Server.TCPPool.IdleTimeout > 0 {
				poolCfg.IdleTimeout = cfg.Server.TCPPool.IdleTimeout
			}
			if cfg.Server.TCPPool.MaxLifetime > 0 {
				poolCfg.MaxLifetime = cfg.Server.TCPPool.MaxLifetime
			}
			b.EnablePooling(&poolCfg)
		} else {
			b.EnablePooling(nil)
		}

		backends = append(backends, b)
	}

	return backends
}

// backendsChanged checks if the backend configuration has changed.
func backendsChanged(old, new *config.Config) bool {
	if len(old.Server.Backends) != len(new.Server.Backends) {
		return true
	}
	for i := range old.Server.Backends {
		if old.Server.Backends[i].Addr != new.Server.Backends[i].Addr {
			return true
		}
		if old.Server.Backends[i].Weight != new.Server.Backends[i].Weight {
			return true
		}
	}
	return false
}
