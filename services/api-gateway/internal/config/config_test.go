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
