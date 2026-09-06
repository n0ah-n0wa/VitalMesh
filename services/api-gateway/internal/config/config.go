// Package config loads and validates the gateway's runtime configuration from
// environment variables. Invalid mandatory configuration is reported as an
// error so that start-up fails instead of running with unsafe values.
package config

import (
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"strconv"
	"strings"
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
	Auth        Auth
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

// Secret is key material. It prints as a placeholder through every common
// formatting path (fmt, %v, %+v, %#v, slog, JSON), so an accidental dump of
// the configuration cannot leak it. Use the bytes directly where the value
// is needed.
type Secret []byte

const redacted = "[redacted]"

func (Secret) String() string   { return redacted }
func (Secret) GoString() string { return redacted }

// LogValue implements slog.LogValuer.
func (Secret) LogValue() slog.Value { return slog.StringValue(redacted) }

// MarshalText implements encoding.TextMarshaler, which encoding/json uses.
func (Secret) MarshalText() ([]byte, error) { return []byte(redacted), nil }

// Auth configures authentication: access tokens and password hashing.
type Auth struct {
	JWT      JWT
	Password PasswordHash
}

// JWT configures access token signing (HS256) and verification.
type JWT struct {
	// KeyID names the signing key in the `kid` header of every issued token.
	KeyID string
	// Secret is the HMAC key tokens are signed with. It is mandatory and
	// must hold at least MinJWTSecretBytes bytes.
	Secret Secret
	// PreviousSecrets are retired keys, by key ID, that are still accepted
	// for verification so that rotating Secret does not invalidate tokens
	// issued before the rotation.
	PreviousSecrets map[string]Secret
	// Issuer is the `iss` claim written and required.
	Issuer string
	// TTL is the lifetime of an access token.
	TTL time.Duration
	// ClockSkew is the tolerance applied to the time claims.
	ClockSkew time.Duration
}

// PasswordHash holds the Argon2id cost parameters for new password hashes
// and the bound on concurrent hashing operations.
type PasswordHash struct {
	MemoryKiB   uint32
	Time        uint32
	Parallelism uint8
	// MaxConcurrent caps how many hash computations may run at once, so
	// that unauthenticated login attempts cannot commit more than
	// MaxConcurrent * MemoryKiB of memory. Further requests wait for a slot
	// until their request deadline.
	MaxConcurrent int
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

// Bounds enforced on authentication settings.
const (
	MinJWTSecretBytes = 32
	MaxJWTTTL         = 24 * time.Hour
	MaxJWTClockSkew   = 5 * time.Minute
	MaxJWTKeyIDLength = 64

	MinPasswordHashMemoryKiB  = 8 * 1024
	MaxPasswordHashMemoryKiB  = 1024 * 1024
	MaxPasswordHashTime       = 100
	MaxPasswordHashThreads    = 64
	MaxPasswordHashConcurrent = 1024
)

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
		Auth: Auth{
			JWT:      p.jwt(),
			Password: p.passwordHash(),
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
	if cfg.Environment.Deployed() && cfg.Database.URL != "" && !databaseURLRequiresTLS(cfg.Database.URL) {
		p.fail("DATABASE_URL: sslmode must be require, verify-ca or verify-full in %s (SPECIFICATIONS.md section 30)", cfg.Environment)
	}

	if err := p.err(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

// Deployed reports whether the environment is one where traffic leaves the
// developer's machine and transport security is mandatory.
func (e Environment) Deployed() bool {
	return e == Staging || e == Production
}

// databaseURLRequiresTLS reports whether a libpq connection string (URL or
// key=value form) demands TLS: sslmode require, verify-ca or verify-full.
// The default and the opportunistic modes (allow, prefer) fall back to
// plaintext silently, so they do not count.
func databaseURLRequiresTLS(dsn string) bool {
	mode := ""
	if u, err := url.Parse(dsn); err == nil && (u.Scheme == "postgres" || u.Scheme == "postgresql") {
		mode = u.Query().Get("sslmode")
	} else {
		for _, kv := range strings.Fields(dsn) {
			if v, ok := strings.CutPrefix(kv, "sslmode="); ok {
				mode = strings.Trim(v, `'"`)
			}
		}
	}
	switch mode {
	case "require", "verify-ca", "verify-full":
		return true
	}
	return false
}

// LoadPasswordHash reads only the password hashing parameters, for command
// line tools that create accounts without running the server.
func LoadPasswordHash(lookup Lookup) (PasswordHash, error) {
	p := parser{lookup: lookup}
	ph := p.passwordHash()
	if err := p.err(); err != nil {
		return PasswordHash{}, err
	}
	return ph, nil
}

func (p *parser) jwt() JWT {
	cfg := JWT{
		KeyID:     p.string("JWT_KEY_ID", "1"),
		Secret:    Secret(p.required("JWT_SECRET")),
		Issuer:    p.string("JWT_ISSUER", "vitalmesh"),
		TTL:       p.duration("JWT_TTL", 15*time.Minute),
		ClockSkew: p.duration("JWT_CLOCK_SKEW", 30*time.Second),
	}
	if !validKeyID(cfg.KeyID) {
		p.fail("JWT_KEY_ID: must be 1 to %d characters of [A-Za-z0-9._-]", MaxJWTKeyIDLength)
	}
	if len(cfg.Secret) > 0 && len(cfg.Secret) < MinJWTSecretBytes {
		p.fail("JWT_SECRET: must be at least %d bytes", MinJWTSecretBytes)
	}
	if cfg.Issuer == "" || strings.ContainsAny(cfg.Issuer, " \t\r\n") {
		p.fail("JWT_ISSUER: must not be empty or contain whitespace")
	}
	if cfg.TTL > MaxJWTTTL {
		p.fail("JWT_TTL: must be at most %s, got %s", MaxJWTTTL, cfg.TTL)
	}
	if cfg.ClockSkew > MaxJWTClockSkew {
		p.fail("JWT_CLOCK_SKEW: must be at most %s, got %s", MaxJWTClockSkew, cfg.ClockSkew)
	}
	cfg.PreviousSecrets = p.previousSecrets("JWT_PREVIOUS_SECRETS", cfg.KeyID)
	return cfg
}

// previousSecrets parses "kid=secret,kid=secret". Secrets therefore cannot
// contain commas; the active secret has no such restriction.
func (p *parser) previousSecrets(key, activeKeyID string) map[string]Secret {
	raw, ok := p.raw(key)
	if !ok {
		return nil
	}
	out := make(map[string]Secret)
	for _, entry := range strings.Split(raw, ",") {
		kid, secret, found := strings.Cut(entry, "=")
		switch {
		case !found:
			p.fail("%s: entries must be of the form kid=secret", key)
		case !validKeyID(kid):
			p.fail("%s: key id must be 1 to %d characters of [A-Za-z0-9._-]", key, MaxJWTKeyIDLength)
		case kid == activeKeyID:
			p.fail("%s: key id %q is the active JWT_KEY_ID", key, kid)
		case len(secret) < MinJWTSecretBytes:
			p.fail("%s: secret for key id %q must be at least %d bytes", key, kid, MinJWTSecretBytes)
		default:
			if _, dup := out[kid]; dup {
				p.fail("%s: key id %q is listed twice", key, kid)
			}
			out[kid] = Secret(secret)
		}
	}
	return out
}

func (p *parser) passwordHash() PasswordHash {
	cfg := PasswordHash{
		MemoryKiB:     p.uint32("PASSWORD_HASH_MEMORY_KIB", 64*1024),
		Time:          p.uint32("PASSWORD_HASH_TIME", 3),
		MaxConcurrent: int(p.uint32("PASSWORD_HASH_MAX_CONCURRENT", 4)),
	}
	if cfg.MaxConcurrent > MaxPasswordHashConcurrent {
		p.fail("PASSWORD_HASH_MAX_CONCURRENT: must be at most %d", MaxPasswordHashConcurrent)
		cfg.MaxConcurrent = 4
	}
	parallelism := p.uint32("PASSWORD_HASH_PARALLELISM", 1)
	if parallelism > MaxPasswordHashThreads {
		p.fail("PASSWORD_HASH_PARALLELISM: must be at most %d", MaxPasswordHashThreads)
		parallelism = 1
	}
	cfg.Parallelism = uint8(parallelism)
	if cfg.MemoryKiB < MinPasswordHashMemoryKiB || cfg.MemoryKiB > MaxPasswordHashMemoryKiB {
		p.fail("PASSWORD_HASH_MEMORY_KIB: must be between %d and %d", MinPasswordHashMemoryKiB, MaxPasswordHashMemoryKiB)
	}
	if cfg.Time > MaxPasswordHashTime {
		p.fail("PASSWORD_HASH_TIME: must be at most %d", MaxPasswordHashTime)
	}
	return cfg
}

func validKeyID(id string) bool {
	if id == "" || len(id) > MaxJWTKeyIDLength {
		return false
	}
	for i := 0; i < len(id); i++ {
		c := id[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '.', c == '_', c == '-':
		default:
			return false
		}
	}
	return true
}

type parser struct {
	lookup Lookup
	errs   []error
}

func (p *parser) err() error {
	if len(p.errs) == 0 {
		return nil
	}
	return fmt.Errorf("invalid configuration:\n%w", errors.Join(p.errs...))
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

func (p *parser) uint32(key string, def uint32) uint32 {
	v, ok := p.raw(key)
	if !ok {
		return def
	}
	n, err := strconv.ParseUint(v, 10, 32)
	if err != nil {
		p.fail("%s: must be a positive integer", key)
		return def
	}
	if n == 0 {
		p.fail("%s: must be positive", key)
		return def
	}
	return uint32(n)
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
