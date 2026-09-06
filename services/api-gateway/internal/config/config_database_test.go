package config

import (
	"strings"
	"testing"
	"time"
)

func TestDatabaseURLIsMandatory(t *testing.T) {
	_, err := Load(func(string) (string, bool) { return "", false })
	if err == nil || !strings.Contains(err.Error(), "DATABASE_URL: is required") {
		t.Fatalf("Load without DATABASE_URL: err = %v", err)
	}
}

func TestDatabaseSettings(t *testing.T) {
	cfg, err := Load(lookupFrom(map[string]string{
		"DATABASE_URL":             "postgres://u:p@db:5432/vm?sslmode=require",
		"DATABASE_MAX_CONNS":       "25",
		"DATABASE_CONNECT_TIMEOUT": "750ms",
	}))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Database.URL != "postgres://u:p@db:5432/vm?sslmode=require" {
		t.Errorf("URL = %q", cfg.Database.URL)
	}
	if cfg.Database.MaxConns != 25 || cfg.Database.ConnectTimeout != 750*time.Millisecond {
		t.Errorf("pool settings = %+v", cfg.Database)
	}

	defaults, err := Load(lookupFrom(nil))
	if err != nil {
		t.Fatalf("Load defaults: %v", err)
	}
	if defaults.Database.MaxConns != 10 || defaults.Database.ConnectTimeout != 5*time.Second {
		t.Errorf("defaults = %+v", defaults.Database)
	}

	_, err = Load(lookupFrom(map[string]string{"DATABASE_MAX_CONNS": "0"}))
	if err == nil || !strings.Contains(err.Error(), "DATABASE_MAX_CONNS: must be positive") {
		t.Fatalf("zero pool size: err = %v", err)
	}
}
