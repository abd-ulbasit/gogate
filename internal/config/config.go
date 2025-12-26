package config

import (
	"errors"
	"fmt"
	"os"
	"time"

	"gopkg.in/yaml.v3"
)

// Config represents the proxy configuration.
// For Week 1, we keep it simple: single backend.
type Config struct {
	Server ServerConfig `yaml:"server"`
}

type ServerConfig struct {
	// ListenAddr is the address to listen on (e.g., ":8080")
	ListenAddr string `yaml:"listen"`

	// Backends is the list of backend servers to load balance across
	Backends []BackendConfig `yaml:"backends"`

	// LoadBalancer selects the algorithm: "round_robin", "weighted_round_robin", or "least_connections"
	LoadBalancer string `yaml:"load_balancer"`

	// Health config for active checks (optional)
	Health HealthConfig `yaml:"health"`

	// RateLimit config for global rate limiting (optional)
	RateLimit RateLimitConfig `yaml:"rate_limit"`

	// CircuitBreaker config for per-backend circuit breakers (optional)
	CircuitBreaker CircuitBreakerConfig `yaml:"circuit_breaker"`

	// Admin config for admin HTTP endpoint (optional)
	Admin AdminConfig `yaml:"admin"`

	// TCPPool enables TCP connection pooling to backends.
	TCPPool TCPPoolConfig `yaml:"tcp_pool"`

	// HTTPTransport configures HTTP connection pooling/timeouts for HTTP proxying.
	HTTPTransport HTTPTransportConfig `yaml:"http_transport"`

	// Registry configures the service discovery registry.
	Registry RegistryConfig `yaml:"registry"`

	// Logging configures structured logging.
	Logging LoggingConfig `yaml:"logging"`

	// Metrics configures Prometheus metrics endpoint.
	Metrics MetricsConfig `yaml:"metrics"`
}

// BackendConfig represents a single backend entry in config.
type BackendConfig struct {
	Addr   string `yaml:"addr"`
	Weight int    `yaml:"weight"`
}

// HealthConfig mirrors CheckerConfig defaults; zero values are replaced by defaults.
type HealthConfig struct {
	Interval           time.Duration `yaml:"interval"`
	Timeout            time.Duration `yaml:"timeout"`
	UnhealthyThreshold int           `yaml:"unhealthy_threshold"`
	HealthyThreshold   int           `yaml:"healthy_threshold"`
	CheckType          string        `yaml:"check_type"`
	HTTPPath           string        `yaml:"http_path"`
}

// RateLimitConfig controls global rate limiting.
type RateLimitConfig struct {
	Enabled bool    `yaml:"enabled"` // Enable rate limiting
	Rate    float64 `yaml:"rate"`    // Requests per second
	Burst   int     `yaml:"burst"`   // Max burst size
}

// CircuitBreakerConfig controls per-backend circuit breakers.
type CircuitBreakerConfig struct {
	Enabled          bool          `yaml:"enabled"`           // Enable circuit breakers
	FailureThreshold int           `yaml:"failure_threshold"` // Failures before open
	SuccessThreshold int           `yaml:"success_threshold"` // Successes in half-open to close
	Timeout          time.Duration `yaml:"timeout"`           // Time before half-open
	Window           time.Duration `yaml:"window"`            // Failure counting window
}

// TCPPoolConfig controls backend TCP connection pooling.
type TCPPoolConfig struct {
	Enabled     bool          `yaml:"enabled"`
	MaxIdle     int           `yaml:"max_idle"`
	IdleTimeout time.Duration `yaml:"idle_timeout"`
	MaxLifetime time.Duration `yaml:"max_lifetime"`
}

// HTTPTransportConfig controls HTTP client pooling/timeouts for proxying.
type HTTPTransportConfig struct {
	MaxIdleConns          int           `yaml:"max_idle_conns"`
	MaxIdleConnsPerHost   int           `yaml:"max_idle_conns_per_host"`
	MaxConnsPerHost       int           `yaml:"max_conns_per_host"`
	IdleConnTimeout       time.Duration `yaml:"idle_conn_timeout"`
	TLSHandshakeTimeout   time.Duration `yaml:"tls_handshake_timeout"`
	ExpectContinueTimeout time.Duration `yaml:"expect_continue_timeout"`
	DialTimeout           time.Duration `yaml:"dial_timeout"`
	DialKeepAlive         time.Duration `yaml:"dial_keepalive"`
	DisableCompression    bool          `yaml:"disable_compression"`
}

// RegistryConfig controls the service discovery registry.
type RegistryConfig struct {
	Enabled         bool          `yaml:"enabled"`          // Enable service registry
	Listen          string        `yaml:"listen"`           // Address for registry API (e.g., ":8500")
	DefaultTTL      time.Duration `yaml:"default_ttl"`      // Default TTL for services
	CleanupInterval time.Duration `yaml:"cleanup_interval"` // How often to clean expired services
	MaxServices     int           `yaml:"max_services"`     // Max number of services (0 = unlimited)
}

// AdminConfig controls the admin HTTP endpoint.
type AdminConfig struct {
	Enabled bool   `yaml:"enabled"` // Enable admin endpoint
	Listen  string `yaml:"listen"`  // Address for admin server (e.g., ":9090")
}

// LoggingConfig controls structured logging.
type LoggingConfig struct {
	Level     string `yaml:"level"`      // Log level: debug, info, warn, error (default: info)
	Format    string `yaml:"format"`     // Log format: json, text (default: json)
	AddSource bool   `yaml:"add_source"` // Include source file:line in logs (default: false)
}

// MetricsConfig controls Prometheus metrics endpoint.
type MetricsConfig struct {
	Enabled bool   `yaml:"enabled"` // Enable /metrics endpoint
	Listen  string `yaml:"listen"`  // Address for metrics server (default: same as admin)
	Path    string `yaml:"path"`    // Metrics path (default: /metrics)
}

func (c *Config) applyDefaults() {
	// Default load balancer
	if c.Server.LoadBalancer == "" {
		c.Server.LoadBalancer = "round_robin"
	}

	// Default weights
	for i := range c.Server.Backends {
		if c.Server.Backends[i].Weight == 0 {
			c.Server.Backends[i].Weight = 1
		}
	}

	// Health defaults
	if c.Server.Health.Interval == 0 {
		c.Server.Health.Interval = 5 * time.Second
	}
	if c.Server.Health.Timeout == 0 {
		c.Server.Health.Timeout = 2 * time.Second
	}
	if c.Server.Health.UnhealthyThreshold == 0 {
		c.Server.Health.UnhealthyThreshold = 3
	}
	if c.Server.Health.HealthyThreshold == 0 {
		c.Server.Health.HealthyThreshold = 2
	}
	if c.Server.Health.CheckType == "" {
		c.Server.Health.CheckType = "tcp"
	}
	if c.Server.Health.HTTPPath == "" {
		c.Server.Health.HTTPPath = "/health"
	}

	// Rate limit defaults (only if enabled)
	if c.Server.RateLimit.Enabled {
		if c.Server.RateLimit.Rate == 0 {
			c.Server.RateLimit.Rate = 1000 // 1000 req/sec
		}
		if c.Server.RateLimit.Burst == 0 {
			c.Server.RateLimit.Burst = 100 // Burst of 100
		}
	}

	// Circuit breaker defaults (only if enabled)
	if c.Server.CircuitBreaker.Enabled {
		if c.Server.CircuitBreaker.FailureThreshold == 0 {
			c.Server.CircuitBreaker.FailureThreshold = 5
		}
		if c.Server.CircuitBreaker.SuccessThreshold == 0 {
			c.Server.CircuitBreaker.SuccessThreshold = 1
		}
		if c.Server.CircuitBreaker.Timeout == 0 {
			c.Server.CircuitBreaker.Timeout = 30 * time.Second
		}
		if c.Server.CircuitBreaker.Window == 0 {
			c.Server.CircuitBreaker.Window = 60 * time.Second
		}
	}

	// Admin defaults
	if c.Server.Admin.Enabled && c.Server.Admin.Listen == "" {
		c.Server.Admin.Listen = ":9090"
	}

	// TCP pool defaults (only if enabled)
	if c.Server.TCPPool.Enabled {
		if c.Server.TCPPool.MaxIdle == 0 {
			c.Server.TCPPool.MaxIdle = 64
		}
		if c.Server.TCPPool.IdleTimeout == 0 {
			c.Server.TCPPool.IdleTimeout = 30 * time.Second
		}
		if c.Server.TCPPool.MaxLifetime == 0 {
			c.Server.TCPPool.MaxLifetime = 2 * time.Minute
		}
	}

	// HTTP transport defaults
	if c.Server.HTTPTransport.MaxIdleConns == 0 {
		c.Server.HTTPTransport.MaxIdleConns = 100
	}
	if c.Server.HTTPTransport.MaxIdleConnsPerHost == 0 {
		c.Server.HTTPTransport.MaxIdleConnsPerHost = 10
	}
	if c.Server.HTTPTransport.IdleConnTimeout == 0 {
		c.Server.HTTPTransport.IdleConnTimeout = 90 * time.Second
	}
	if c.Server.HTTPTransport.TLSHandshakeTimeout == 0 {
		c.Server.HTTPTransport.TLSHandshakeTimeout = 10 * time.Second
	}
	if c.Server.HTTPTransport.ExpectContinueTimeout == 0 {
		c.Server.HTTPTransport.ExpectContinueTimeout = 1 * time.Second
	}
	if c.Server.HTTPTransport.DialTimeout == 0 {
		c.Server.HTTPTransport.DialTimeout = 30 * time.Second
	}
	if c.Server.HTTPTransport.DialKeepAlive == 0 {
		c.Server.HTTPTransport.DialKeepAlive = 30 * time.Second
	}

	// Registry defaults (only if enabled)
	if c.Server.Registry.Enabled {
		if c.Server.Registry.Listen == "" {
			c.Server.Registry.Listen = ":8500"
		}
		if c.Server.Registry.DefaultTTL == 0 {
			c.Server.Registry.DefaultTTL = 30 * time.Second
		}
		if c.Server.Registry.CleanupInterval == 0 {
			c.Server.Registry.CleanupInterval = 10 * time.Second
		}
	}

	// Logging defaults
	if c.Server.Logging.Level == "" {
		c.Server.Logging.Level = "info"
	}
	if c.Server.Logging.Format == "" {
		c.Server.Logging.Format = "json"
	}

	// Metrics defaults (only if enabled)
	if c.Server.Metrics.Enabled {
		if c.Server.Metrics.Path == "" {
			c.Server.Metrics.Path = "/metrics"
		}
		// If no separate listen addr, metrics will be served on admin server
	}
}

// Load reads configuration from a YAML file.
//
// Hint: Use os.ReadFile to read the file
// Hint: Use yaml.Unmarshal to parse YAML into Config struct
// Hint: Validate that required fields are set
func Load(path string) (*Config, error) {
	// Step 1: Read file
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}

	// Step 2: Parse YAML
	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}

	cfg.applyDefaults()

	// Step 3: Validate
	if cfg.Server.ListenAddr == "" {
		return nil, errors.New("server.listen is required")
	}
	if len(cfg.Server.Backends) == 0 {
		return nil, errors.New("server.backends must have at least one backend")
	}
	for i, b := range cfg.Server.Backends {
		if b.Addr == "" {
			return nil, fmt.Errorf("server.backends[%d].addr is required", i)
		}
		if b.Weight < 0 {
			return nil, fmt.Errorf("server.backends[%d].weight must be >= 0", i)
		}
	}

	return &cfg, nil

}

// Validate checks that the configuration is valid.
func (c *Config) Validate() error {
	if c.Server.ListenAddr == "" {
		return fmt.Errorf("server.listen is required")
	}
	if len(c.Server.Backends) == 0 {
		return fmt.Errorf("server.backends must have at least one backend")
	}
	for i, b := range c.Server.Backends {
		if b.Addr == "" {
			return fmt.Errorf("server.backends[%d].addr is required", i)
		}
		if b.Weight < 0 {
			return fmt.Errorf("server.backends[%d].weight must be >= 0", i)
		}
	}
	return nil
}

// HTTPTransportConfigValues returns the HTTP transport config with defaults applied.
func (c *Config) HTTPTransportConfigValues() HTTPTransportConfig {
	return c.Server.HTTPTransport
}
