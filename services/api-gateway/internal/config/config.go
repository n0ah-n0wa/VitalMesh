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
	Environment  Environment
	HTTP         HTTP
	Database     Database
	Auth         Auth
	Measurements Measurements
	Processing   Processing
	Processor    Processor
	Idempotency  Idempotency
	Redis        Redis
	Tracing      Tracing
	RateLimit    RateLimit
	Cache        Cache
	Readiness    Readiness
	Log          Log
}

// Tracing configures distributed tracing (SPECIFICATIONS.md section 40).
//
// Tracing is optional by design. With no endpoint the gateway still takes
// part in a trace: it reads and forwards W3C trace context, so a trace
// started by a client survives the hop, and the trace id still reaches the
// logs. What it does not do is export spans of its own.
type Tracing struct {
	// Endpoint is the OTLP/HTTP collector, scheme and host only. Empty
	// means no exporter.
	Endpoint string
	// SampleRatio is the fraction of traces recorded, between 0 and 1. A
	// sampling decision made upstream is always honoured, so this only
	// decides for traces that start here.
	SampleRatio float64
	// Timeout bounds one export attempt.
	Timeout time.Duration
	// Insecure permits plain HTTP to the collector. It is refused outside
	// local development, where a collector is reached over the network.
	Insecure bool
}

// Enabled reports whether spans are exported.
func (t Tracing) Enabled() bool { return t.Endpoint != "" }

// Redis configures the shared ephemeral store (SPECIFICATIONS.md section
// 23). Redis is never a record of truth: it holds rate-limit counters,
// short-lived cache entries and idempotency locks, all of which the gateway
// can lose without losing correctness. Leaving REDIS_URL empty disables it
// and every feature that uses it degrades to its local behaviour.
type Redis struct {
	// URL is a redis:// or rediss:// address. Empty means no Redis.
	URL string
	// DialTimeout bounds establishing a connection.
	DialTimeout time.Duration
	// Timeout bounds one command, read or write. Every Redis call the
	// gateway makes is on a request's critical path, so this is short: a
	// slow Redis must degrade, not slow the API down.
	Timeout time.Duration
	// PoolSize is the largest number of connections held.
	PoolSize int
	// Namespace prefixes every key, so several environments can share one
	// server without colliding.
	Namespace string
	// RecoveryInterval is how long the client waits after a failure before
	// trying Redis again. Between failure and recovery, calls fail fast
	// locally rather than spending their timeout on a dead server.
	RecoveryInterval time.Duration
}

// Enabled reports whether Redis is configured.
func (r Redis) Enabled() bool { return r.URL != "" }

// RateLimit configures distributed rate limiting (SPECIFICATIONS.md section
// 29). Limits are per role and per window and are never hard-coded.
type RateLimit struct {
	// Enabled turns rate limiting on. It works without Redis, using a
	// per-replica limiter, which is weaker but never absent.
	Enabled bool
	// Window is the period each limit applies to.
	Window time.Duration
	// Anonymous applies to requests with no authenticated principal.
	Anonymous int
	// Authenticated applies to any signed-in role that has no more
	// specific limit.
	Authenticated int
	// Admin applies to the ADMIN role.
	Admin int
	// TrustedProxyHops is how many reverse proxies sit in front of the
	// gateway. It decides which entry of X-Forwarded-For is the client;
	// zero means the header is ignored and the socket address is used, so
	// a client cannot choose its own rate-limit identity.
	TrustedProxyHops int
}

// Cache configures short-lived caching (SPECIFICATIONS.md section 84).
type Cache struct {
	// Enabled turns caching on. Without Redis every read goes to the
	// database, which is always correct and merely slower.
	Enabled bool
	// PatientTTL bounds how long a patient record may be served from the
	// cache. Writes invalidate the entry explicitly; the TTL bounds the
	// staleness left by an invalidation that could not be delivered.
	PatientTTL time.Duration
}

// Processing bounds the Processing API (SPECIFICATIONS.md sections 13 and
// 89).
type Processing struct {
	// MaxJobMeasurements is the largest number of readings one job may
	// carry. It must not exceed the processor's own bound, which the
	// processor advertises on its health endpoint.
	MaxJobMeasurements int
	// AlgorithmVersion is the version of the processing algorithms this
	// gateway asks for. The processor refuses any other version, so this is
	// how a rolling upgrade is coordinated.
	AlgorithmVersion string
	// ServiceVersion is recorded on a job when it is dispatched, before the
	// processor's own version is known.
	ServiceVersion string
	// FailureRecordTimeout bounds the write that records why a job failed.
	// It is short and separate from the request's own deadline, because the
	// record matters most exactly when that deadline has passed.
	FailureRecordTimeout time.Duration
}

// Processor configures the client for the Rust processing service
// (SPECIFICATIONS.md sections 8 and 37).
type Processor struct {
	// BaseURL is the processor's address, without a path.
	BaseURL string
	// Token is the shared secret presented as `Authorization: Bearer`.
	Token Secret
	// Timeout bounds one attempt. It must be shorter than the HTTP request
	// timeout, or a client would give up before the gateway could answer.
	Timeout time.Duration
	// MaxAttempts is how many times one dispatch may be tried, including
	// the first. Retries stop early when the caller's deadline leaves no
	// room for another attempt.
	MaxAttempts int
	// Backoff is the delay before the second attempt; it doubles with each
	// further attempt, up to MaxBackoff.
	Backoff    time.Duration
	MaxBackoff time.Duration
	// ContractVersion is the internal contract this gateway is built
	// against, reported in diagnostics.
	ContractVersion string
}

// Measurements bounds measurement ingestion (SPECIFICATIONS.md sections 12,
// 17 and 89).
type Measurements struct {
	// MaxBatchSize is the largest number of readings one batch request may
	// carry.
	MaxBatchSize int
	// MaxMetadataBytes bounds a reading's metadata object, measured as
	// compact JSON. The schema caps the stored form at 4096 bytes.
	MaxMetadataBytes int
	// MaxFutureSkew is how far ahead of the server clock recorded_at may lie.
	MaxFutureSkew time.Duration
}

// Idempotency configures Idempotency-Key handling (section 24).
type Idempotency struct {
	// TTL is how long a recorded request stays replayable.
	TTL time.Duration
	// RetentionInterval is how often expired records are swept. Sweeping
	// is what stops the table growing without bound, since a key is
	// usually never presented twice.
	RetentionInterval time.Duration
}

// Bounds enforced on measurement and idempotency settings.
// DefaultAlgorithmVersion is the processing algorithm version this build of
// the gateway asks the processor for. It matches the processor's
// anomaly::ALGORITHM_VERSION; a mismatch is refused by the processor rather
// than producing results nobody can compare.
const DefaultAlgorithmVersion = "1.0.0"

// InternalContractVersion is the version of
// contracts/internal-api/processor-v1.json this gateway is built against.
const InternalContractVersion = "1.1.1"

// Bounds enforced on Redis-backed settings.
const (
	MaxRedisNamespaceLength = 64
	// MinRateLimitWindow keeps a window long enough that its counter is
	// meaningful; MaxRateLimitWindow keeps the memory a window occupies
	// bounded.
	MinRateLimitWindow = time.Second
	MaxRateLimitWindow = time.Hour
	// MaxTrustedProxyHops bounds how far back through X-Forwarded-For the
	// client address may be taken.
	MaxTrustedProxyHops = 8
	// MaxCacheTTL keeps cached data short-lived, so that a missed
	// invalidation cannot outlive it by much (SPECIFICATIONS.md section 84).
	MaxCacheTTL = 5 * time.Minute
)

// Bounds enforced on processing settings.
const (
	// MaxProcessingJobMeasurements is the ceiling on the configured job
	// size. It matches the processor's own default bound.
	MaxProcessingJobMeasurements = 1000000
	// MaxProcessorAttempts bounds the retry budget, so that a
	// misconfiguration cannot turn one client request into a storm
	// (SPECIFICATIONS.md section 93).
	MaxProcessorAttempts = 5
)

const (
	MaxMeasurementBatchSize    = 10000
	MaxMeasurementMetadataSize = 4096
	MaxMeasurementFutureSkew   = time.Hour
	MinIdempotencyTTL          = time.Minute
	MaxIdempotencyTTL          = 7 * 24 * time.Hour
	// Bounds on the retention sweep: often enough that expired records do
	// not pile up, rarely enough that the sweep is not itself load.
	MinRetentionInterval = time.Second
	MaxRetentionInterval = 24 * time.Hour
)

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
		Measurements: Measurements{
			MaxBatchSize:     int(p.uint32("MEASUREMENT_MAX_BATCH_SIZE", 1000)),
			MaxMetadataBytes: int(p.uint32("MEASUREMENT_MAX_METADATA_BYTES", 2048)),
			MaxFutureSkew:    p.duration("MEASUREMENT_MAX_FUTURE_SKEW", 5*time.Minute),
		},
		Processing: Processing{
			MaxJobMeasurements:   int(p.uint32("PROCESSING_MAX_JOB_MEASUREMENTS", 100000)),
			AlgorithmVersion:     p.string("PROCESSING_ALGORITHM_VERSION", DefaultAlgorithmVersion),
			ServiceVersion:       p.string("PROCESSING_SERVICE_VERSION", "api-gateway"),
			FailureRecordTimeout: p.duration("PROCESSING_FAILURE_RECORD_TIMEOUT", 5*time.Second),
		},
		Processor: Processor{
			BaseURL:         p.string("PROCESSOR_URL", "http://127.0.0.1:8081"),
			Token:           Secret(p.string("PROCESSOR_TOKEN", "")),
			Timeout:         p.duration("PROCESSOR_TIMEOUT", 5*time.Second),
			MaxAttempts:     int(p.uint32("PROCESSOR_MAX_ATTEMPTS", 3)),
			Backoff:         p.duration("PROCESSOR_BACKOFF", 100*time.Millisecond),
			MaxBackoff:      p.duration("PROCESSOR_MAX_BACKOFF", 2*time.Second),
			ContractVersion: InternalContractVersion,
		},
		Idempotency: Idempotency{
			TTL:               p.duration("IDEMPOTENCY_TTL", 24*time.Hour),
			RetentionInterval: p.duration("IDEMPOTENCY_RETENTION_INTERVAL", time.Hour),
		},
		Redis: Redis{
			URL:              p.string("REDIS_URL", ""),
			DialTimeout:      p.duration("REDIS_DIAL_TIMEOUT", 2*time.Second),
			Timeout:          p.duration("REDIS_TIMEOUT", 250*time.Millisecond),
			PoolSize:         int(p.uint32("REDIS_POOL_SIZE", 10)),
			Namespace:        p.string("REDIS_NAMESPACE", "vitalmesh"),
			RecoveryInterval: p.duration("REDIS_RECOVERY_INTERVAL", 5*time.Second),
		},
		RateLimit: RateLimit{
			Enabled:          p.bool("RATE_LIMIT_ENABLED", true),
			Window:           p.duration("RATE_LIMIT_WINDOW", time.Minute),
			Anonymous:        int(p.uint32("RATE_LIMIT_ANONYMOUS", 60)),
			Authenticated:    int(p.uint32("RATE_LIMIT_AUTHENTICATED", 300)),
			Admin:            int(p.uint32("RATE_LIMIT_ADMIN", 1000)),
			TrustedProxyHops: int(p.count("TRUSTED_PROXY_HOPS", 0)),
		},
		Tracing: Tracing{
			Endpoint:    p.string("OTEL_EXPORTER_OTLP_ENDPOINT", ""),
			SampleRatio: p.ratio("OTEL_TRACES_SAMPLER_ARG", 1.0),
			Timeout:     p.duration("OTEL_EXPORTER_OTLP_TIMEOUT", 10*time.Second),
			Insecure:    p.bool("OTEL_EXPORTER_OTLP_INSECURE", false),
		},
		Cache: Cache{
			Enabled:    p.bool("CACHE_ENABLED", true),
			PatientTTL: p.duration("CACHE_PATIENT_TTL", 30*time.Second),
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
	if cfg.Measurements.MaxBatchSize > MaxMeasurementBatchSize {
		p.fail("MEASUREMENT_MAX_BATCH_SIZE: must be at most %d", MaxMeasurementBatchSize)
	}
	if cfg.Measurements.MaxMetadataBytes > MaxMeasurementMetadataSize {
		p.fail("MEASUREMENT_MAX_METADATA_BYTES: must be at most %d", MaxMeasurementMetadataSize)
	}
	if cfg.Measurements.MaxFutureSkew > MaxMeasurementFutureSkew {
		p.fail("MEASUREMENT_MAX_FUTURE_SKEW: must be at most %s", MaxMeasurementFutureSkew)
	}
	if cfg.Idempotency.TTL < MinIdempotencyTTL || cfg.Idempotency.TTL > MaxIdempotencyTTL {
		p.fail("IDEMPOTENCY_TTL: must be between %s and %s", MinIdempotencyTTL, MaxIdempotencyTTL)
	}
	if cfg.Idempotency.RetentionInterval < MinRetentionInterval || cfg.Idempotency.RetentionInterval > MaxRetentionInterval {
		p.fail("IDEMPOTENCY_RETENTION_INTERVAL: must be between %s and %s", MinRetentionInterval, MaxRetentionInterval)
	}
	if cfg.Environment.Deployed() && cfg.Database.URL != "" && !databaseURLRequiresTLS(cfg.Database.URL) {
		p.fail("DATABASE_URL: sslmode must be require, verify-ca or verify-full in %s (SPECIFICATIONS.md section 30)", cfg.Environment)
	}
	if cfg.Redis.URL != "" {
		if err := validRedisURL(cfg.Redis.URL); err != nil {
			p.fail("REDIS_URL: %v", err)
		}
		// A Redis call sits on a request's critical path, so it must not be
		// able to outlast the request itself.
		if cfg.Redis.Timeout >= cfg.HTTP.RequestTimeout {
			p.fail("REDIS_TIMEOUT: must be shorter than HTTP_REQUEST_TIMEOUT (%s >= %s)",
				cfg.Redis.Timeout, cfg.HTTP.RequestTimeout)
		}
		if cfg.Redis.PoolSize < 1 {
			p.fail("REDIS_POOL_SIZE: must be at least 1")
		}
		if !validNamespace(cfg.Redis.Namespace) {
			p.fail("REDIS_NAMESPACE: must be 1 to %d characters of [A-Za-z0-9._-]", MaxRedisNamespaceLength)
		}
	}
	if cfg.Environment.Deployed() && cfg.Redis.URL != "" && !strings.HasPrefix(cfg.Redis.URL, "rediss://") {
		p.fail("REDIS_URL: must use rediss:// in %s (SPECIFICATIONS.md section 30)", cfg.Environment)
	}
	if cfg.RateLimit.Enabled {
		for name, limit := range map[string]int{
			"RATE_LIMIT_ANONYMOUS":     cfg.RateLimit.Anonymous,
			"RATE_LIMIT_AUTHENTICATED": cfg.RateLimit.Authenticated,
			"RATE_LIMIT_ADMIN":         cfg.RateLimit.Admin,
		} {
			if limit < 1 {
				p.fail("%s: must be at least 1 request per window", name)
			}
		}
		if cfg.RateLimit.Window < MinRateLimitWindow || cfg.RateLimit.Window > MaxRateLimitWindow {
			p.fail("RATE_LIMIT_WINDOW: must be between %s and %s", MinRateLimitWindow, MaxRateLimitWindow)
		}
		if cfg.RateLimit.TrustedProxyHops > MaxTrustedProxyHops {
			p.fail("TRUSTED_PROXY_HOPS: must be at most %d", MaxTrustedProxyHops)
		}
	}
	if cfg.Tracing.Endpoint != "" {
		if err := validEndpoint(cfg.Tracing.Endpoint); err != nil {
			p.fail("OTEL_EXPORTER_OTLP_ENDPOINT: %v", err)
		}
		if cfg.Tracing.SampleRatio < 0 || cfg.Tracing.SampleRatio > 1 {
			p.fail("OTEL_TRACES_SAMPLER_ARG: must be between 0 and 1")
		}
		if cfg.Environment.Deployed() && cfg.Tracing.Insecure {
			p.fail("OTEL_EXPORTER_OTLP_INSECURE: spans must not cross the network in the clear in %s (SPECIFICATIONS.md section 30)", cfg.Environment)
		}
	}
	if cfg.Cache.Enabled && (cfg.Cache.PatientTTL < time.Second || cfg.Cache.PatientTTL > MaxCacheTTL) {
		p.fail("CACHE_PATIENT_TTL: must be between 1s and %s", MaxCacheTTL)
	}
	if cfg.Processing.MaxJobMeasurements < 1 || cfg.Processing.MaxJobMeasurements > MaxProcessingJobMeasurements {
		p.fail("PROCESSING_MAX_JOB_MEASUREMENTS: must be between 1 and %d", MaxProcessingJobMeasurements)
	}
	if cfg.Processing.AlgorithmVersion == "" {
		p.fail("PROCESSING_ALGORITHM_VERSION: must not be empty")
	}
	if err := processorclientValidateBaseURL(cfg.Processor.BaseURL); err != nil {
		p.fail("PROCESSOR_URL: %v", err)
	}
	// A client that gives up before the gateway can answer turns a clear
	// failure into a mystery, so the processor's bound must fit inside the
	// request's.
	if cfg.Processor.Timeout >= cfg.HTTP.RequestTimeout {
		p.fail("PROCESSOR_TIMEOUT: must be shorter than HTTP_REQUEST_TIMEOUT (%s >= %s)",
			cfg.Processor.Timeout, cfg.HTTP.RequestTimeout)
	}
	if cfg.Processor.MaxAttempts < 1 || cfg.Processor.MaxAttempts > MaxProcessorAttempts {
		p.fail("PROCESSOR_MAX_ATTEMPTS: must be between 1 and %d", MaxProcessorAttempts)
	}
	if cfg.Processor.MaxBackoff < cfg.Processor.Backoff {
		p.fail("PROCESSOR_MAX_BACKOFF: must be at least PROCESSOR_BACKOFF (%s < %s)",
			cfg.Processor.MaxBackoff, cfg.Processor.Backoff)
	}
	// The internal API must not be reachable without a credential where it
	// carries real traffic (SPECIFICATIONS.md sections 30 and 31).
	if cfg.Environment.Deployed() && len(cfg.Processor.Token) == 0 {
		p.fail("PROCESSOR_TOKEN: required in %s, where the processor must not be called without a credential", cfg.Environment)
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

// validEndpoint checks a collector address without connecting to it. Like
// the Redis check it never echoes the address, which may carry a
// credential.
func validEndpoint(raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return errors.New("must be a valid URL")
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return errors.New("must be an http:// or https:// URL")
	}
	if u.Host == "" {
		return errors.New("must name a host")
	}
	return nil
}

// validRedisURL checks a Redis address without connecting to it.
//
// It never returns the parse error, which quotes the address it rejected.
// A Redis URL may carry a password and this message reaches the start-up
// log, so the reason is described rather than shown (SPECIFICATIONS.md
// section 31).
func validRedisURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return errors.New("must be a valid URL")
	}
	if u.Scheme != "redis" && u.Scheme != "rediss" {
		return errors.New("must be a redis:// or rediss:// URL")
	}
	if u.Host == "" {
		return errors.New("must name a host")
	}
	return nil
}

// validNamespace accepts a short key prefix.
func validNamespace(ns string) bool {
	if ns == "" || len(ns) > MaxRedisNamespaceLength {
		return false
	}
	for i := 0; i < len(ns); i++ {
		c := ns[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '.', c == '_', c == '-':
		default:
			return false
		}
	}
	return true
}

// processorclientValidateBaseURL checks the processor address. The check
// lives here rather than in the client package because config must not
// depend on a package that depends on config.
func processorclientValidateBaseURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return err
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return errors.New("must be an http or https URL")
	}
	if u.Host == "" {
		return errors.New("must name a host")
	}
	if u.Path != "" && u.Path != "/" {
		return errors.New("must not carry a path")
	}
	return nil
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

// bool accepts the spellings Go's strconv accepts, so "1", "true" and
// "TRUE" all turn a feature on.
func (p *parser) bool(key string, def bool) bool {
	v, ok := p.raw(key)
	if !ok {
		return def
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		p.fail("%s: must be true or false", key)
		return def
	}
	return b
}

// count is uint32 for values where zero is meaningful, such as a number of
// proxy hops.
func (p *parser) count(key string, def uint32) uint32 {
	v, ok := p.raw(key)
	if !ok {
		return def
	}
	n, err := strconv.ParseUint(v, 10, 32)
	if err != nil {
		p.fail("%s: must be a non-negative integer", key)
		return def
	}
	return uint32(n)
}

// ratio is a fraction between 0 and 1, such as a sampling rate.
func (p *parser) ratio(key string, def float64) float64 {
	v, ok := p.raw(key)
	if !ok {
		return def
	}
	f, err := strconv.ParseFloat(v, 64)
	if err != nil {
		p.fail("%s: must be a number between 0 and 1", key)
		return def
	}
	return f
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
