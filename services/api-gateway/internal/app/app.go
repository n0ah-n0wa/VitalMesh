// Package app assembles the gateway from its parts and runs its lifecycle.
// It is the only package that knows every other package.
package app

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/n0ah-n0wa/VitalMesh/services/api-gateway/internal/auth"
	"github.com/n0ah-n0wa/VitalMesh/services/api-gateway/internal/authz"
	"github.com/n0ah-n0wa/VitalMesh/services/api-gateway/internal/buildinfo"
	"github.com/n0ah-n0wa/VitalMesh/services/api-gateway/internal/cache"
	"github.com/n0ah-n0wa/VitalMesh/services/api-gateway/internal/config"
	"github.com/n0ah-n0wa/VitalMesh/services/api-gateway/internal/domain"
	"github.com/n0ah-n0wa/VitalMesh/services/api-gateway/internal/health"
	"github.com/n0ah-n0wa/VitalMesh/services/api-gateway/internal/httpapi"
	"github.com/n0ah-n0wa/VitalMesh/services/api-gateway/internal/httpapi/handler"
	"github.com/n0ah-n0wa/VitalMesh/services/api-gateway/internal/httpapi/middleware"
	"github.com/n0ah-n0wa/VitalMesh/services/api-gateway/internal/idempotency"
	"github.com/n0ah-n0wa/VitalMesh/services/api-gateway/internal/infra/postgres"
	"github.com/n0ah-n0wa/VitalMesh/services/api-gateway/internal/infra/processorclient"
	"github.com/n0ah-n0wa/VitalMesh/services/api-gateway/internal/infra/redisclient"
	"github.com/n0ah-n0wa/VitalMesh/services/api-gateway/internal/measurement"
	"github.com/n0ah-n0wa/VitalMesh/services/api-gateway/internal/observability/metrics"
	"github.com/n0ah-n0wa/VitalMesh/services/api-gateway/internal/observability/tracing"
	"github.com/n0ah-n0wa/VitalMesh/services/api-gateway/internal/patient"
	"github.com/n0ah-n0wa/VitalMesh/services/api-gateway/internal/processing"
	"github.com/n0ah-n0wa/VitalMesh/services/api-gateway/internal/ratelimit"
	"github.com/n0ah-n0wa/VitalMesh/services/api-gateway/internal/retention"
)

// ServiceName identifies the gateway in logs and health responses.
const ServiceName = "api-gateway"

// App is a fully wired gateway.
type App struct {
	cfg     config.Config
	logger  *slog.Logger
	pool    *pgxpool.Pool
	redis   *redisclient.Client
	handler http.Handler
	// traces is shut down after the server stops, so what was recorded is
	// flushed rather than lost.
	traces *tracing.Provider
	// retention removes expired idempotency records for as long as the
	// gateway is running.
	retention *idempotency.Collector
	// sweeper fails jobs whose lease expired: jobs a gateway that stopped
	// mid-dispatch left PROCESSING.
	sweeper *processing.Sweeper
	// data removes readings and results past their retention window
	// (SPECIFICATIONS.md section 81). Disabled unless configured, in which
	// case it returns immediately and removes nothing.
	data *retention.Sweeper
}

// New wires the application. The database pool connects lazily, so New
// succeeds even when PostgreSQL is down; readiness reports the outage.
func New(ctx context.Context, cfg config.Config, logger *slog.Logger, version string) (*App, error) {
	// Observability ports. The observability phase binds Prometheus and
	// OpenTelemetry here; until then nothing is recorded.
	// One recorder, one registry, built before anything that reports to it.
	prom := metrics.NewPrometheus()
	var rec metrics.Recorder = prom

	// What this build is, published once. Every value is a version or a
	// digest; nothing here describes the machine or the deployment.
	build := buildinfo.Current()
	prom.SetBuildInfo(metrics.Build{
		Version:          build.Version,
		GoVersion:        build.GoVersion,
		Modules:          build.Modules,
		AlgorithmVersion: cfg.Processing.AlgorithmVersion,
		ImageDigest:      build.ImageDigest,
	})

	// Tracing is built before anything that reports to it. It installs the
	// W3C propagator whether or not spans are exported, so trace context
	// crosses this service even with no collector configured.
	traces, err := tracing.NewProvider(cfg.Tracing, tracing.Service{
		Name: ServiceName, Version: version, Environment: string(cfg.Environment),
	}, logger)
	if err != nil {
		return nil, fmt.Errorf("tracing: %w", err)
	}
	tr := traces.Tracer()
	if !cfg.Tracing.Enabled() {
		logger.Info("OTEL_EXPORTER_OTLP_ENDPOINT is not set: trace context is propagated but no spans are exported")
	}
	pool, err := postgres.Connect(ctx, cfg.Database, postgres.Options{Metrics: rec, Tracer: tr})
	if err != nil {
		return nil, fmt.Errorf("database: %w", err)
	}

	// The migration version this process starts against, published so that
	// two replicas disagreeing mid-rollout is visible. Best effort: the
	// pool connects lazily, so a database that is down at start-up leaves
	// the metric absent rather than delaying the start or reporting zero.
	func() {
		probe, cancel := context.WithTimeout(ctx, 2*time.Second)
		defer cancel()
		switch version, dirty, found, err := postgres.SchemaVersion(probe, pool); {
		case err != nil:
			logger.WarnContext(ctx, "schema version not read at start-up", "error", err)
		case !found:
			logger.WarnContext(ctx, "no migration has been applied to this database")
		default:
			prom.SetSchemaVersion(version)
			// Not "version": the logger already puts the service's own
			// version on every record, and two keys of that name in one
			// JSON object is a collision a log consumer resolves by
			// guessing.
			logger.InfoContext(ctx, "schema version", "schema_version", version, "dirty", dirty)
		}
	}()

	// Redis is optional by design (SPECIFICATIONS.md sections 23 and 90).
	// It connects lazily, so start-up succeeds while it is down and picks
	// it up when it returns; only an unusable address stops the gateway,
	// because that is a configuration mistake rather than an outage.
	redis, err := redisclient.New(cfg.Redis, logger, redisclient.Options{Metrics: rec, Tracer: tr})
	if err != nil {
		pool.Close()
		return nil, fmt.Errorf("redis: %w", err)
	}
	if !cfg.Redis.Enabled() {
		logger.Warn("REDIS_URL is not set: rate limiting uses a per-replica counter, " +
			"caching is off and idempotency relies on the database alone")
	}
	// The pool's own saturation, which is what separates a slow database
	// from a gateway that has run out of connections to it.
	prom.Registry().MustRegister(postgres.PoolCollector(pool))

	patientCache := cache.NewRedis(redis, cfg.Cache.Enabled, logger)
	limiter := ratelimit.New(cfg.RateLimit, redis, logger, ratelimit.Options{Metrics: rec})

	// Only PostgreSQL decides readiness. Redis being down degrades three
	// features and stops none of them, so failing readiness for it would
	// take a working gateway out of rotation (OPEN_QUESTIONS OQ-27).
	readiness := health.NewReadiness(cfg.Readiness.Timeout, postgres.NewChecker(pool))
	tokens := auth.NewTokens(cfg.Auth.JWT, nil)
	authService, err := auth.NewService(
		postgres.NewUsers(pool),
		auditor{repo: postgres.NewAudit(pool)},
		auth.NewHasher(cfg.Auth.Password),
		tokens,
		logger,
	)
	if err != nil {
		pool.Close()
		return nil, fmt.Errorf("auth: %w", err)
	}

	idempotencyStore := postgres.NewIdempotencyStore(pool)
	patients := patient.NewService(postgres.NewPatientStore(pool), logger, patient.Options{
		Metrics: rec, Tracer: tr, Cache: patientCache, CacheTTL: cfg.Cache.PatientTTL,
	})
	measurements := measurement.NewService(postgres.NewMeasurementStore(pool), cfg.Measurements, logger, measurement.Options{Metrics: rec, Tracer: tr})
	processor := processorclient.New(cfg.Processor, logger, processorclient.Options{Tracer: tr})
	jobStore := postgres.NewJobStore(pool, postgres.JobStoreOptions{Lease: cfg.Processing.JobLease})
	jobs := processing.NewService(jobStore, processor, cfg.Processing, logger, processing.Options{Metrics: rec, Tracer: tr})

	handlers := httpapi.Handlers{
		Health:         handler.NewHealth(ServiceName, version, string(cfg.Environment), readiness, logger),
		Auth:           handler.NewAuth(authService, logger),
		Patients:       handler.NewPatients(patients, logger),
		Measurements:   handler.NewMeasurements(measurements, logger),
		Processing:     handler.NewProcessing(jobs, logger),
		Authenticate:   middleware.Authenticate(tokens, logger),
		Idempotency:    middleware.Idempotency(idempotencyStore, cfg.Idempotency.TTL, redis, logger),
		RateLimit:      middleware.RateLimit(limiter, logger),
		Policy:         authz.Default(),
		Metrics:        rec,
		Tracer:         tr,
		MetricsHandler: prom.Handler(),
	}
	root, err := httpapi.NewHandler(cfg.HTTP, logger, handlers)
	if err != nil {
		pool.Close()
		_ = redis.Close()
		_ = traces.Shutdown(ctx)
		return nil, fmt.Errorf("routes: %w", err)
	}
	return &App{
		cfg:       cfg,
		logger:    logger,
		pool:      pool,
		redis:     redis,
		handler:   root,
		traces:    traces,
		retention: idempotency.NewCollector(idempotencyStore, cfg.Idempotency.RetentionInterval, logger, idempotency.CollectorOptions{}),
		sweeper:   processing.NewSweeper(jobStore, cfg.Processing.LeaseSweepInterval, logger, processing.SweeperOptions{}),
		data: retention.New(
			retentionStore{retention: postgres.NewRetention(pool), audit: postgres.NewAudit(pool)},
			cfg.Retention, logger, retention.Options{Metrics: rec},
		),
	}, nil
}

// Handler returns the root HTTP handler, for in-process tests.
func (a *App) Handler() http.Handler { return a.handler }

// Run serves HTTP until ctx is cancelled and the server has shut down, then
// closes the database pool.
func (a *App) Run(ctx context.Context) error {
	// Closed in the order the specification's shutdown sequence gives
	// (section 38): connections are released after the server has stopped
	// serving, Redis before the database because nothing depends on it.
	defer a.pool.Close()
	defer func() {
		// Bounded: flushing must not hang on a collector that has gone away,
		// and an export that cannot finish is not a reason to delay exit.
		flush, cancel := context.WithTimeout(context.WithoutCancel(ctx), a.cfg.HTTP.ShutdownTimeout)
		defer cancel()
		if err := a.traces.Shutdown(flush); err != nil {
			a.logger.Warn("recorded spans were not flushed", "error", err)
		}
	}()
	defer func() {
		if err := a.redis.Close(); err != nil {
			a.logger.Warn("redis connections not closed cleanly", "error", err)
		}
	}()

	// Retention and the lease sweep run alongside the server and stop with
	// it. Their work is bounded per pass, so shutdown waits on at most one
	// batch of each.
	background, stopBackground := context.WithCancel(ctx)
	var sweeping sync.WaitGroup
	sweeping.Add(3)
	go func() {
		defer sweeping.Done()
		a.retention.Run(background)
	}()
	go func() {
		defer sweeping.Done()
		a.sweeper.Run(background)
	}()
	go func() {
		defer sweeping.Done()
		a.data.Run(background)
	}()
	// Deferred last-in-first-out: cancel, then wait for the sweeps to stop.
	defer sweeping.Wait()
	defer stopBackground()

	a.logger.Info("starting", "addr", a.cfg.HTTP.Addr)
	if err := httpapi.Run(ctx, a.handler, a.cfg.HTTP); err != nil {
		return err
	}
	a.logger.Info("stopped")
	return nil
}

// retentionStore adapts two repositories to the one port the retention
// sweep needs, so that package does not depend on persistence types.
type retentionStore struct {
	retention *postgres.Retention
	audit     *postgres.Audit
}

func (s retentionStore) DeleteMeasurementsBefore(ctx context.Context, cutoff time.Time, limit int) (int64, error) {
	return s.retention.DeleteMeasurementsBefore(ctx, cutoff, limit)
}

func (s retentionStore) DeleteResultsBefore(ctx context.Context, cutoff time.Time, limit int) (int64, error) {
	return s.retention.DeleteResultsBefore(ctx, cutoff, limit)
}

// RecordRetentionRun appends the entry section 81 requires. The actor is
// the system rather than a user, and there is no request behind it, so the
// request id is the empty string the column allows.
func (s retentionStore) RecordRetentionRun(ctx context.Context, metadata json.RawMessage) error {
	_, err := s.audit.Append(ctx, postgres.NewAuditEntry{
		ActorType:    domain.ActorSystem,
		Action:       retention.Action,
		ResourceType: "retention",
		Metadata:     metadata,
	})
	return err
}

// auditor adapts the audit repository to the events the auth service
// records, keeping auth free of persistence types.
type auditor struct {
	repo *postgres.Audit
}

func (a auditor) RecordLogin(ctx context.Context, userID uuid.UUID, requestID string) error {
	_, err := a.repo.Append(ctx, postgres.NewAuditEntry{
		ActorID:      &userID,
		ActorType:    domain.ActorUser,
		Action:       domain.AuditLogin,
		ResourceType: "user",
		ResourceID:   &userID,
		RequestID:    requestID,
	})
	return err
}
