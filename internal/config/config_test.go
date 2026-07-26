package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// repoRoot resolves the repository root from this package's directory.
func repoRoot(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	return filepath.Clean(filepath.Join(wd, "..", ".."))
}

// TestShippedConfigsLoad parses every configuration file the project ships and
// asserts it produces a config the binary would actually accept.
//
// This exists because YAML unmarshalling is silent about keys it does not
// recognise. A config written with the wrong key style - `listenAddr` instead of
// `listen`, `rps` instead of `rate` - parses without error into a zero-valued
// struct, and the only symptom is the process refusing to start, or worse,
// starting with a feature the operator believes is enabled quietly switched off.
// Nothing about the file looks wrong when you read it.
//
// Adding a config file to the repo without adding it here is the mistake this
// guards against, so the list is explicit rather than a glob.
func TestShippedConfigsLoad(t *testing.T) {
	root := repoRoot(t)

	cases := []struct {
		file            string
		wantHTTPListen  bool
		wantRateLimit   bool
		wantCircuitBrk  bool
		wantMinBackends int
	}{
		{
			file:            "config.example.yaml",
			wantHTTPListen:  true,
			wantRateLimit:   true,
			wantCircuitBrk:  true,
			wantMinBackends: 1,
		},
		{
			file:            "config.docker.yaml",
			wantHTTPListen:  true,
			wantRateLimit:   true,
			wantCircuitBrk:  true,
			wantMinBackends: 1,
		},
		{
			file:            filepath.Join("deployments", "docker", "config-docker.yaml"),
			wantHTTPListen:  true,
			wantRateLimit:   true,
			wantCircuitBrk:  true,
			wantMinBackends: 1,
		},
	}

	for _, tc := range cases {
		t.Run(tc.file, func(t *testing.T) {
			path := filepath.Join(root, tc.file)
			cfg, err := Load(path)
			if err != nil {
				t.Fatalf("Load(%s): %v", tc.file, err)
			}
			if err := cfg.Validate(); err != nil {
				t.Fatalf("Validate(%s): %v", tc.file, err)
			}

			if got := len(cfg.Server.Backends); got < tc.wantMinBackends {
				t.Errorf("backends parsed = %d, want >= %d (wrong key name silently yields none)",
					got, tc.wantMinBackends)
			}
			if tc.wantHTTPListen && cfg.Server.HTTPListenAddr == "" {
				t.Error("http_listen did not parse: the L7 listener would silently not start")
			}
			if tc.wantRateLimit && !cfg.Server.RateLimit.Enabled {
				t.Error("rate_limit.enabled did not parse: limiter would be silently off")
			}
			if tc.wantRateLimit && cfg.Server.RateLimit.Rate <= 0 {
				t.Errorf("rate_limit.rate = %v, want > 0", cfg.Server.RateLimit.Rate)
			}
			if tc.wantCircuitBrk && !cfg.Server.CircuitBreaker.Enabled {
				t.Error("circuit_breaker.enabled did not parse: breakers would be silently off")
			}
			if tc.wantCircuitBrk && cfg.Server.CircuitBreaker.Timeout <= 0 {
				t.Errorf("circuit_breaker.timeout = %v, want > 0", cfg.Server.CircuitBreaker.Timeout)
			}
			if cfg.Server.LoadBalancer == "" {
				t.Error("load_balancer is empty after defaults")
			}
		})
	}
}

// TestHelmConfigMapKeysMatchSchema checks the Helm chart's rendered config keys
// against the struct tags. The template is not valid YAML on its own, so this
// compares key names textually rather than unmarshalling.
func TestHelmConfigMapKeysMatchSchema(t *testing.T) {
	root := repoRoot(t)
	path := filepath.Join(root, "deployments", "helm", "gogate", "templates", "configmap.yaml")

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read configmap template: %v", err)
	}
	text := string(data)

	// Keys the loader understands. A camelCase spelling of any of these parses
	// into nothing and disables the feature without an error.
	wantKeys := []string{
		"listen:",
		"http_listen:",
		"load_balancer:",
		"unhealthy_threshold:",
		"healthy_threshold:",
		"check_type:",
		"rate_limit:",
		"circuit_breaker:",
		"failure_threshold:",
		"success_threshold:",
		"tcp_pool:",
		"max_idle:",
		"idle_timeout:",
	}
	for _, key := range wantKeys {
		if !strings.Contains(text, key) {
			t.Errorf("configmap template is missing schema key %q", key)
		}
	}

	// camelCase spellings that silently unmarshal to nothing.
	badKeys := []string{
		"listenAddr:",
		"httpListenAddr:",
		"loadBalancer:",
		"rateLimit:",
		"circuitBreaker:",
		"failureThreshold:",
		"successThreshold:",
		"unhealthyThreshold:",
		"healthyThreshold:",
		"checkType:",
		"tcpPool:",
		"maxIdle:",
		"idleTimeout:",
		"maxLifetime:",
	}
	for _, key := range badKeys {
		if strings.Contains(text, key) {
			t.Errorf("configmap template uses %q, which the loader ignores; the pod would start with the feature off", key)
		}
	}
}

// TestUnknownKeysAreIgnored documents the loader behaviour the tests above exist
// to compensate for: yaml.v3 does not reject unknown fields by default.
//
// A misspelled required key is caught, because Load validates it. A misspelled
// optional key is not caught by anything - the feature is simply off, with no
// error and no log line. That asymmetry is why the shipped configs are tested
// by assertion rather than by "it loaded, ship it".
func TestUnknownKeysAreIgnored(t *testing.T) {
	dir := t.TempDir()

	t.Run("misspelled required key fails loudly", func(t *testing.T) {
		path := filepath.Join(dir, "required.yaml")
		// `listenAddr` is not the schema key; `listen` is.
		content := "server:\n  listenAddr: \":8080\"\n  backends:\n    - addr: \"127.0.0.1:9001\"\n"
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatalf("write: %v", err)
		}
		if _, err := Load(path); err == nil {
			t.Fatal("Load accepted a config whose listen address key was misspelled")
		}
	})

	t.Run("misspelled optional key fails silently", func(t *testing.T) {
		path := filepath.Join(dir, "optional.yaml")
		// `rateLimit` is not the schema key; `rate_limit` is.
		content := "server:\n  listen: \":8080\"\n  backends:\n    - addr: \"127.0.0.1:9001\"\n" +
			"  rateLimit:\n    enabled: true\n    rate: 1000\n"
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatalf("write: %v", err)
		}
		cfg, err := Load(path)
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if cfg.Server.RateLimit.Enabled {
			t.Fatal("rateLimit was picked up; this test no longer describes the loader")
		}
		// No error, no warning: the operator believes rate limiting is on.
		t.Log("confirmed: an unrecognised optional key disables the feature silently")
	})
}
