// Package config loads and validates the gateway's runtime configuration from
// environment variables. Invalid mandatory configuration is reported as an
// error so that start-up fails instead of running with unsafe values.
package config

import (
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"time"
)

// Environment identifies the deployment environment.
type Environment string

// Supported environments.
const (
	Local      Environment = "local"
	Test       Environment = "test"
	Staging    Environment = "staging"
	Production Environment = "production"
)

// LogFormat selects the log encoding.
type LogFormat string

// Supported log formats.
const (
	LogFormatJSON LogFormat = "json"
	LogFormatText LogFormat = "text"
)

// Config is the complete, validated configuration of the gateway.
type Config struct {
	Environment Environment
	HTTP        HTTP
	Database    Database
	Readiness   Readiness
	Log         Log
}

// Database configures the PostgreSQL connection pool.
type Database struct {
	// URL is a libpq-style connection URL. It is mandatory.
	URL            string
	MaxConns       int32
	ConnectTimeout time.Duration
}

// HTTP configures the public HTTP listener and per-request protection.
type HTTP struct {
	Addr              string
	ReadHeaderTimeout time.Duration
	ReadTimeout       time.Duration
	WriteTimeout      time.Duration
	IdleTimeout       time.Duration
	ShutdownTimeout   time.Duration
	// RequestTimeout bounds handler execution. It must be shorter than
	// WriteTimeout so that the timeout response can still be written.
	RequestTimeout time.Duration
	// MaxBodyBytes is the largest request body accepted.
	MaxBodyBytes int64
}

// Readiness configures dependency checks behind /ready.
type Readiness struct {
	// Timeout bounds the whole readiness evaluation, all checks included.
	Timeout time.Duration
}

// Log configures structured logging.
type Log struct {
	Level  slog.Level
	Format LogFormat
}

// Lookup returns the value of a configuration key and whether it was set.
// os.LookupEnv satisfies it; tests supply their own.
type Lookup func(key string) (string, bool)

// Load reads configuration through lookup, applying defaults for unset or
// empty keys. Every invalid value is reported; the returned error lists all
// of them.
func Load(lookup Lookup) (Config, error) {
	p := parser{lookup: lookup}

	cfg := Config{
		Environment: Environment(p.string("ENVIRONMENT", string(Local))),
		HTTP: HTTP{
			Addr:              p.string("HTTP_ADDR", ":8080"),
			ReadHeaderTimeout: p.duration("HTTP_READ_HEADER_TIMEOUT", 5*time.Second),
			ReadTimeout:       p.duration("HTTP_READ_TIMEOUT", 10*time.Second),
			WriteTimeout:      p.duration("HTTP_WRITE_TIMEOUT", 15*time.Second),
			IdleTimeout:       p.duration("HTTP_IDLE_TIMEOUT", 60*time.Second),
			ShutdownTimeout:   p.duration("HTTP_SHUTDOWN_TIMEOUT", 10*time.Second),
			RequestTimeout:    p.duration("HTTP_REQUEST_TIMEOUT", 10*time.Second),
			MaxBodyBytes:      p.bytes("HTTP_MAX_BODY_BYTES", 1<<20),
		},
		Database: Database{
			URL:            p.required("DATABASE_URL"),
			MaxConns:       p.int32("DATABASE_MAX_CONNS", 10),
			ConnectTimeout: p.duration("DATABASE_CONNECT_TIMEOUT", 5*time.Second),
		},
		Readiness: Readiness{
			Timeout: p.duration("READINESS_TIMEOUT", 2*time.Second),
		},
		Log: Log{
			Level:  p.level("LOG_LEVEL", slog.LevelInfo),
			Format: LogFormat(p.string("LOG_FORMAT", string(LogFormatJSON))),
		},
	}

	switch cfg.Environment {
	case Local, Test, Staging, Production:
	default:
		p.fail("ENVIRONMENT: unknown value %q", cfg.Environment)
	}
	switch cfg.Log.Format {
	case LogFormatJSON, LogFormatText:
	default:
		p.fail("LOG_FORMAT: unknown value %q", cfg.Log.Format)
	}
	if cfg.HTTP.RequestTimeout >= cfg.HTTP.WriteTimeout {
		p.fail("HTTP_REQUEST_TIMEOUT: must be shorter than HTTP_WRITE_TIMEOUT (%s >= %s)",
			cfg.HTTP.RequestTimeout, cfg.HTTP.WriteTimeout)
	}

	if len(p.errs) > 0 {
		return Config{}, fmt.Errorf("invalid configuration:\n%w", errors.Join(p.errs...))
	}
	return cfg, nil
}

type parser struct {
	lookup Lookup
	errs   []error
}

func (p *parser) fail(format string, args ...any) {
	p.errs = append(p.errs, fmt.Errorf(format, args...))
}

func (p *parser) raw(key string) (string, bool) {
	v, ok := p.lookup(key)
	if !ok || v == "" {
		return "", false
	}
	return v, true
}

func (p *parser) string(key, def string) string {
	if v, ok := p.raw(key); ok {
		return v
	}
	return def
}

func (p *parser) required(key string) string {
	v, ok := p.raw(key)
	if !ok {
		p.fail("%s: is required", key)
	}
	return v
}

func (p *parser) int32(key string, def int32) int32 {
	v, ok := p.raw(key)
	if !ok {
		return def
	}
	n, err := strconv.ParseInt(v, 10, 32)
	if err != nil {
		p.fail("%s: must be an integer", key)
		return def
	}
	if n <= 0 {
		p.fail("%s: must be positive, got %d", key, n)
		return def
	}
	return int32(n)
}

func (p *parser) duration(key string, def time.Duration) time.Duration {
	v, ok := p.raw(key)
	if !ok {
		return def
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		p.fail("%s: %v", key, err)
		return def
	}
	if d <= 0 {
		p.fail("%s: must be positive, got %s", key, d)
		return def
	}
	return d
}

func (p *parser) bytes(key string, def int64) int64 {
	v, ok := p.raw(key)
	if !ok {
		return def
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil {
		p.fail("%s: must be an integer number of bytes", key)
		return def
	}
	if n <= 0 {
		p.fail("%s: must be positive, got %d", key, n)
		return def
	}
	return n
}

func (p *parser) level(key string, def slog.Level) slog.Level {
	v, ok := p.raw(key)
	if !ok {
		return def
	}
	var l slog.Level
	if err := l.UnmarshalText([]byte(v)); err != nil {
		p.fail("%s: %v", key, err)
		return def
	}
	return l
}
