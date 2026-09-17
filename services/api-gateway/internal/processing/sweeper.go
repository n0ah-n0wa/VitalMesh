package processing

import (
	"context"
	"log/slog"
	"time"
)

// LeaseExpirer fails jobs whose lease has expired. It is a separate port
// from [Store] because the request path never needs it and the sweep never
// needs the rest.
type LeaseExpirer interface {
	// FailExpiredJobs moves at most limit jobs whose lease expired at or
	// before now to FAILED with the given diagnosis, and reports how many.
	FailExpiredJobs(ctx context.Context, now time.Time, failure Failure, limit int) (int64, error)
}

// SweepBatch is how many jobs one statement fails. It bounds the
// transaction and the locks it takes, so a sweep never blocks the write
// path it shares a table with.
const SweepBatch = 100

// maxBatchesPerSweep stops one pass from running unboundedly long. What is
// left is simply failed by the next pass.
const maxBatchesPerSweep = 100

// InterruptedFailure is the diagnosis a swept job carries (SPECIFICATIONS.md
// section 94): a stable code and a message safe to show.
func InterruptedFailure() Failure {
	return Failure{Code: CodeProcessingInterrupted, Message: "The job was interrupted before it could finish."}
}

// Sweeper fails jobs whose lease has expired.
//
// A dispatch moves a job to PROCESSING and holds it under a lease longer
// than any legitimate dispatch can last. A job still PROCESSING after its
// lease has expired belongs to a gateway that stopped before finishing it,
// which graceful shutdown cannot prevent when the stop is a crash, an OOM
// kill or a lost node. Without this the job would stay in flight for ever,
// which is neither a terminal state (section 92) nor a failure record
// (section 94). The sweep fails it with PROCESSING_INTERRUPTED and keeps
// everything else about it.
//
// The sweep is safe to run on every replica: each pass fails a bounded set
// of rows it locks first, so two replicas sweeping at once share the work,
// and a job whose lease is still valid is never touched however slow its
// gateway is being.
type Sweeper struct {
	expirer  LeaseExpirer
	interval time.Duration
	logger   *slog.Logger
	now      func() time.Time
}

// SweeperOptions tune a Sweeper. Nil fields take safe defaults.
type SweeperOptions struct {
	Now func() time.Time
}

// NewSweeper returns a sweeper that runs every interval.
func NewSweeper(expirer LeaseExpirer, interval time.Duration, logger *slog.Logger, opts SweeperOptions) *Sweeper {
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	return &Sweeper{expirer: expirer, interval: interval, logger: logger, now: now}
}

// Run sweeps until ctx ends. It returns when the context is done, so the
// caller can wait for it during shutdown.
//
// A failed sweep is logged and retried at the next tick rather than
// escalated: a database that cannot be reached is already reported by
// readiness, and the jobs will still be there.
func (s *Sweeper) Run(ctx context.Context) {
	ticker := time.NewTicker(s.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if _, err := s.Sweep(ctx); err != nil && ctx.Err() == nil {
				s.logger.WarnContext(ctx, "processing lease sweep failed", "error", err)
			}
		}
	}
}

// Sweep fails expired jobs in batches until none are left, and reports how
// many it failed. It stops early when the context ends, so shutdown is
// never delayed by a backlog.
func (s *Sweeper) Sweep(ctx context.Context) (int64, error) {
	var total int64
	for batch := 0; batch < maxBatchesPerSweep; batch++ {
		if err := ctx.Err(); err != nil {
			return total, err
		}
		failed, err := s.expirer.FailExpiredJobs(ctx, s.now(), InterruptedFailure(), SweepBatch)
		total += failed
		if err != nil {
			return total, err
		}
		if failed < SweepBatch {
			break
		}
	}
	if total > 0 {
		// Worth a warning: every one of these is a gateway that stopped
		// mid-job, which shutdown should have prevented.
		s.logger.WarnContext(ctx, "interrupted processing jobs failed by the lease sweep", "count", total)
	}
	return total, nil
}
