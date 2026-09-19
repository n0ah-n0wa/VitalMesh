// Package retention removes stored synthetic data that has outlived its
// configured window (SPECIFICATIONS.md section 81).
//
// Section 81 asks for three things and this package is each of them:
// retention is configurable, removal happens without breaking referential
// integrity, and the job that does it is observable and auditable.
//
// # What is removed, and what is not
//
// Readings and processing results are removed; processing jobs are not.
// A job row is small and bounded by how many were ever requested, and
// section 94 requires a failed job to keep its diagnosis rather than
// disappear, so the record of what was asked for and what became of it
// survives the data it was about. Nothing holds a foreign key to a
// measurement, and a result's key is to its job rather than to the readings
// it was computed from, so neither delete can orphan a row.
//
// # Disabled by default
//
// Both windows default to zero, which removes nothing. Deleting data is
// irreversible, so it happens because an operator asked for it, never
// because nobody set a variable.
package retention

import (
	"context"
	"encoding/json"
	"log/slog"
	"time"

	"github.com/n0ah-n0wa/VitalMesh/services/api-gateway/internal/config"
	"github.com/n0ah-n0wa/VitalMesh/services/api-gateway/internal/domain"
	"github.com/n0ah-n0wa/VitalMesh/services/api-gateway/internal/observability/metrics"
)

// Store is what a sweep needs of persistence. It is a narrow port: the
// request path never removes anything by age, and this job never needs the
// rest of the repositories.
type Store interface {
	// DeleteMeasurementsBefore removes at most limit readings recorded
	// before cutoff and reports how many it removed.
	DeleteMeasurementsBefore(ctx context.Context, cutoff time.Time, limit int) (int64, error)
	// DeleteResultsBefore removes at most limit results created before
	// cutoff and reports how many it removed.
	DeleteResultsBefore(ctx context.Context, cutoff time.Time, limit int) (int64, error)
	// RecordRetentionRun appends the audit entry for one completed pass.
	RecordRetentionRun(ctx context.Context, metadata json.RawMessage) error
}

// maxBatchesPerSweep stops one pass running unboundedly long when a
// backlog has built up, for instance the first pass after retention is
// switched on. What is left is removed by the next pass; nothing depends on
// a sweep finishing the table in one go.
const maxBatchesPerSweep = 200

// Operation names the sweep in metrics and logs.
const Operation = "retention.sweep"

// Sweeper removes data past its retention window on an interval.
//
// It is safe to run on every replica. Each batch deletes a bounded set of
// rows chosen by age and removed by primary key, so two replicas sweeping
// at once simply share the work; a row one has already removed is not
// counted twice, because the count comes from the rows the statement
// actually deleted.
type Sweeper struct {
	store   Store
	cfg     config.Retention
	logger  *slog.Logger
	metrics metrics.Recorder
	now     func() time.Time
}

// Options tune a Sweeper. Nil fields take safe defaults.
type Options struct {
	Now func() time.Time
	// Metrics receives one observation per pass; nil records none.
	Metrics metrics.Recorder
}

// New returns a sweeper for cfg.
func New(store Store, cfg config.Retention, logger *slog.Logger, opts Options) *Sweeper {
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	return &Sweeper{
		store:   store,
		cfg:     cfg,
		logger:  logger,
		metrics: metrics.OrNoop(opts.Metrics),
		now:     now,
	}
}

// Result is what one pass removed.
type Result struct {
	Measurements int64
	Results      int64
}

// Total is how many rows the pass removed altogether.
func (r Result) Total() int64 { return r.Measurements + r.Results }

// Run sweeps until ctx ends. It returns when the context is done, so the
// caller can wait for it during shutdown.
//
// With retention disabled it returns immediately rather than ticking over
// doing nothing, and says so once at startup: an operator reading the logs
// of a service that is quietly keeping everything for ever should be able
// to see that it is on purpose.
func (s *Sweeper) Run(ctx context.Context) {
	if !s.cfg.Enabled() {
		s.logger.InfoContext(ctx, "retention is disabled; stored data is kept indefinitely",
			"measurement_retention_days", s.cfg.MeasurementDays,
			"result_retention_days", s.cfg.ResultDays)
		return
	}
	s.logger.InfoContext(ctx, "retention enabled",
		"measurement_retention_days", s.cfg.MeasurementDays,
		"result_retention_days", s.cfg.ResultDays,
		"interval", s.cfg.Interval)

	ticker := time.NewTicker(s.cfg.Interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if _, err := s.Sweep(ctx); err != nil && ctx.Err() == nil {
				s.logger.WarnContext(ctx, "retention sweep failed", "error", err)
			}
		}
	}
}

// Sweep removes everything past its window and reports what it removed.
//
// A failure part way through is not rolled back and does not need to be:
// each batch is its own statement, the rows it removed were past their
// window, and the next pass continues from where this one stopped. What
// the caller gets back is what was actually removed before the failure.
func (s *Sweeper) Sweep(ctx context.Context) (Result, error) {
	started := s.now()
	var result Result

	if s.cfg.MeasurementDays > 0 {
		cutoff := s.cutoff(s.cfg.MeasurementDays)
		removed, err := s.drain(ctx, cutoff, s.store.DeleteMeasurementsBefore)
		result.Measurements = removed
		if err != nil {
			return s.finish(ctx, started, result, err)
		}
	}
	if s.cfg.ResultDays > 0 {
		cutoff := s.cutoff(s.cfg.ResultDays)
		removed, err := s.drain(ctx, cutoff, s.store.DeleteResultsBefore)
		result.Results = removed
		if err != nil {
			return s.finish(ctx, started, result, err)
		}
	}
	return s.finish(ctx, started, result, nil)
}

// cutoff is the instant before which data of the given age is removed.
func (s *Sweeper) cutoff(days int) time.Time {
	return s.now().UTC().AddDate(0, 0, -days)
}

// drain repeats one delete until it removes fewer rows than it asked for,
// which is how it knows nothing is left, or until the budget or the
// context runs out.
func (s *Sweeper) drain(ctx context.Context, cutoff time.Time, del func(context.Context, time.Time, int) (int64, error)) (int64, error) {
	var total int64
	for batch := 0; batch < maxBatchesPerSweep; batch++ {
		if err := ctx.Err(); err != nil {
			return total, err
		}
		removed, err := del(ctx, cutoff, s.cfg.BatchLimit)
		total += removed
		if err != nil {
			return total, err
		}
		if removed < int64(s.cfg.BatchLimit) {
			return total, nil
		}
	}
	return total, nil
}

// finish records the pass: a metric, a log line, and, when anything was
// removed, the audit entry section 81 requires.
func (s *Sweeper) finish(ctx context.Context, started time.Time, result Result, cause error) (Result, error) {
	outcome := metrics.OutcomeOK
	if cause != nil {
		outcome = metrics.OutcomeError
	}
	s.metrics.Operation(Operation, outcome, s.now().Sub(started))
	s.metrics.Batch("retention.measurements", int(result.Measurements))
	s.metrics.Batch("retention.results", int(result.Results))

	if result.Total() == 0 {
		return result, cause
	}

	s.logger.InfoContext(ctx, "retention removed data past its window",
		"measurements", result.Measurements,
		"results", result.Results,
		"measurement_retention_days", s.cfg.MeasurementDays,
		"result_retention_days", s.cfg.ResultDays)

	// The audit entry carries counts and the windows, never an identifier
	// of anything removed: it says what the policy did, not who it was
	// about (SPECIFICATIONS.md sections 41 and 43).
	metadata, err := json.Marshal(map[string]any{
		"measurements_removed":       result.Measurements,
		"results_removed":            result.Results,
		"measurement_retention_days": s.cfg.MeasurementDays,
		"result_retention_days":      s.cfg.ResultDays,
	})
	if err != nil {
		// Unreachable for this shape, and not worth failing a completed
		// sweep over.
		s.logger.ErrorContext(ctx, "retention audit metadata not encoded", "error", err)
		return result, cause
	}
	if err := s.store.RecordRetentionRun(ctx, metadata); err != nil {
		// The data is gone and the audit entry is not. That is worth an
		// error in the log, and it is not worth replacing the sweep's own
		// outcome, which the caller needs.
		s.logger.ErrorContext(ctx, "retention run not audited", "error", err)
	}
	return result, cause
}

// Action is the audit action a completed pass records. It is declared here
// so the constant in the domain has a caller rather than only a definition.
const Action = domain.AuditRetentionRun
