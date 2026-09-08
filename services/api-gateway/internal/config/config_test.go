package config

import (
	"log/slog"
	"strings"
	"testing"
	"time"
)

func lookupFrom(values map[string]string) Lookup {
	return func(key string) (string, bool) {
		if v, ok := values[key]; ok {
			return v, true
		}
		switch key {
		case "DATABASE_URL":
			return testDatabaseURL, true
		case "JWT_SECRET":
			return testJWTSecret, true
		}
		return "", false
	}
}

// testDatabaseURL and testJWTSecret satisfy the mandatory keys in tests that
// are not about them; nothing connects to or signs with them.
const (
	testDatabaseURL = "postgres://user:pass@localhost:5432/vitalmesh?sslmode=disable"
	testJWTSecret   = "test-secret-test-secret-test-secret-32"
)

func TestLoadDefaults(t *testing.T) {
	cfg, err := Load(lookupFrom(nil))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if cfg.Environment != Local {
		t.Errorf("Environment = %q, want %q", cfg.Environment, Local)
	}
	if cfg.HTTP.Addr != ":8080" {
		t.Errorf("HTTP.Addr = %q, want :8080", cfg.HTTP.Addr)
	}
	if cfg.HTTP.ShutdownTimeout != 10*time.Second {
		t.Errorf("HTTP.ShutdownTimeout = %s, want 10s", cfg.HTTP.ShutdownTimeout)
	}
	if cfg.Readiness.Timeout != 2*time.Second {
		t.Errorf("Readiness.Timeout = %s, want 2s", cfg.Readiness.Timeout)
	}
	if cfg.HTTP.RequestTimeout != 10*time.Second || cfg.HTTP.MaxBodyBytes != 1<<20 {
		t.Errorf("HTTP protection defaults = %s/%d, want 10s/1048576", cfg.HTTP.RequestTimeout, cfg.HTTP.MaxBodyBytes)
	}
	if cfg.HTTP.RequestTimeout >= cfg.HTTP.WriteTimeout {
		t.Errorf("default RequestTimeout %s is not below WriteTimeout %s", cfg.HTTP.RequestTimeout, cfg.HTTP.WriteTimeout)
	}
	if cfg.Log.Level != slog.LevelInfo || cfg.Log.Format != "json" {
		t.Errorf("Log = %+v, want info/json", cfg.Log)
	}
}

func TestLoadRejectsRequestTimeoutAtOrAboveWriteTimeout(t *testing.T) {
	_, err := Load(lookupFrom(map[string]string{"HTTP_REQUEST_TIMEOUT": "15s", "HTTP_WRITE_TIMEOUT": "15s"}))
	if err == nil || !strings.Contains(err.Error(), "HTTP_REQUEST_TIMEOUT: must be shorter than HTTP_WRITE_TIMEOUT") {
		t.Fatalf("err = %v", err)
	}
}

func TestLoadOverrides(t *testing.T) {
	cfg, err := Load(lookupFrom(map[string]string{
		"ENVIRONMENT":           "staging",
		"DATABASE_URL":          "postgres://user:pass@db:5432/vitalmesh?sslmode=require", // staging demands TLS
		"PROCESSOR_TOKEN":       "staging-processor-token",                                // and a processor credential
		"HTTP_ADDR":             "127.0.0.1:9000",
		"HTTP_READ_TIMEOUT":     "3s",
		"HTTP_SHUTDOWN_TIMEOUT": "1m",
		"READINESS_TIMEOUT":     "500ms",
		"LOG_LEVEL":             "debug",
		"LOG_FORMAT":            "text",
	}))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if cfg.Environment != Staging {
		t.Errorf("Environment = %q, want staging", cfg.Environment)
	}
	if cfg.HTTP.Addr != "127.0.0.1:9000" {
		t.Errorf("HTTP.Addr = %q", cfg.HTTP.Addr)
	}
	if cfg.HTTP.ReadTimeout != 3*time.Second {
		t.Errorf("HTTP.ReadTimeout = %s, want 3s", cfg.HTTP.ReadTimeout)
	}
	if cfg.HTTP.ShutdownTimeout != time.Minute {
		t.Errorf("HTTP.ShutdownTimeout = %s, want 1m", cfg.HTTP.ShutdownTimeout)
	}
	if cfg.Readiness.Timeout != 500*time.Millisecond {
		t.Errorf("Readiness.Timeout = %s, want 500ms", cfg.Readiness.Timeout)
	}
	if cfg.Log.Level != slog.LevelDebug || cfg.Log.Format != "text" {
		t.Errorf("Log = %+v, want debug/text", cfg.Log)
	}
}

func TestLoadTreatsEmptyAsUnset(t *testing.T) {
	cfg, err := Load(lookupFrom(map[string]string{"HTTP_ADDR": "", "LOG_LEVEL": ""}))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.HTTP.Addr != ":8080" || cfg.Log.Level != slog.LevelInfo {
		t.Errorf("empty values were not replaced by defaults: %+v", cfg)
	}
}

func TestLoadReportsEveryInvalidValue(t *testing.T) {
	_, err := Load(lookupFrom(map[string]string{
		"ENVIRONMENT":         "prod",
		"HTTP_READ_TIMEOUT":   "soon",
		"HTTP_IDLE_TIMEOUT":   "-1s",
		"HTTP_MAX_BODY_BYTES": "lots",
		"LOG_LEVEL":           "loud",
		"LOG_FORMAT":          "xml",
	}))
	if err == nil {
		t.Fatal("Load returned nil error for invalid configuration")
	}

	for _, want := range []string{
		`ENVIRONMENT: unknown value "prod"`,
		"HTTP_READ_TIMEOUT:",
		"HTTP_IDLE_TIMEOUT: must be positive",
		"HTTP_MAX_BODY_BYTES: must be an integer",
		"LOG_LEVEL:",
		`LOG_FORMAT: unknown value "xml"`,
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
	}
}

// The documented defaults of SPECIFICATIONS.md section 29. They are what a
// deployment that configures nothing gets, so they are worth pinning.
func TestRateLimitDefaults(t *testing.T) {
	cfg, err := Load(lookupFrom(nil))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if !cfg.RateLimit.Enabled {
		t.Error("rate limiting is off by default")
	}
	if cfg.RateLimit.Window != time.Minute {
		t.Errorf("Window = %s, want 1m", cfg.RateLimit.Window)
	}
	limits := []struct {
		name string
		got  int
		want int
	}{
		{"anonymous", cfg.RateLimit.Anonymous, 60},
		{"authenticated", cfg.RateLimit.Authenticated, 300},
		{"admin", cfg.RateLimit.Admin, 1000},
	}
	for _, l := range limits {
		if l.got != l.want {
			t.Errorf("%s limit = %d, want %d per window", l.name, l.got, l.want)
		}
	}
	// A caller must never gain by authenticating less.
	if !(cfg.RateLimit.Anonymous < cfg.RateLimit.Authenticated && cfg.RateLimit.Authenticated < cfg.RateLimit.Admin) {
		t.Errorf("the default limits are not ordered by privilege: %+v", cfg.RateLimit)
	}
	// A client cannot choose its own identity unless a proxy is declared.
	if cfg.RateLimit.TrustedProxyHops != 0 {
		t.Errorf("TrustedProxyHops = %d, want 0", cfg.RateLimit.TrustedProxyHops)
	}
}

func TestRateLimitsAreConfigurable(t *testing.T) {
	cfg, err := Load(lookupFrom(map[string]string{
		"RATE_LIMIT_WINDOW":        "30s",
		"RATE_LIMIT_ANONYMOUS":     "5",
		"RATE_LIMIT_AUTHENTICATED": "50",
		"RATE_LIMIT_ADMIN":         "500",
		"TRUSTED_PROXY_HOPS":       "2",
	}))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	want := RateLimit{
		Enabled: true, Window: 30 * time.Second,
		Anonymous: 5, Authenticated: 50, Admin: 500, TrustedProxyHops: 2,
	}
	if cfg.RateLimit != want {
		t.Errorf("RateLimit = %+v, want %+v", cfg.RateLimit, want)
	}

	off, err := Load(lookupFrom(map[string]string{"RATE_LIMIT_ENABLED": "false"}))
	if err != nil {
		t.Fatalf("Load with rate limiting off: %v", err)
	}
	if off.RateLimit.Enabled {
		t.Error("RATE_LIMIT_ENABLED=false did not turn rate limiting off")
	}
}

func TestRateLimitConfigurationIsBounded(t *testing.T) {
	cases := map[string]map[string]string{
		"a window too short to be meaningful":  {"RATE_LIMIT_WINDOW": "500ms"},
		"a window too long to hold in memory":  {"RATE_LIMIT_WINDOW": "2h"},
		"a limit of zero would refuse callers": {"RATE_LIMIT_ANONYMOUS": "0"},
		"more trusted proxies than are sane":   {"TRUSTED_PROXY_HOPS": "9"},
	}
	for name, values := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := Load(lookupFrom(values)); err == nil {
				t.Errorf("Load accepted %v", values)
			}
		})
	}
}

// A Redis address may carry a password. Nothing that rejects one may quote
// it back: the message reaches the start-up log (SPECIFICATIONS.md section
// 31).
func TestAnInvalidRedisURLIsRejectedWithoutQuotingIt(t *testing.T) {
	const password = "sup3r-s3cret-passphrase"
	cases := map[string]string{
		"unparseable":  "redis://user:" + password + "@host\x7f:6379",
		"wrong scheme": "http://user:" + password + "@host:6379",
		"no host":      "redis://user:" + password + "@",
	}
	for name, raw := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := Load(lookupFrom(map[string]string{"REDIS_URL": raw}))
			if err == nil {
				t.Fatalf("Load accepted %q", name)
			}
			if strings.Contains(err.Error(), password) {
				t.Errorf("the error repeated the password:\n%s", err)
			}
			if strings.Contains(err.Error(), "host") && strings.Contains(err.Error(), "@") {
				t.Errorf("the error repeated the address:\n%s", err)
			}
		})
	}
}
