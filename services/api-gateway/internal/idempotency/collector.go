package idempotency

import (
	"context"
	"log/slog"
	"time"
)

// Expirer deletes records whose time to live has passed. It is a separate
// port from [Store] because the request path never needs it and the
// retention job never needs the rest.
type Expirer interface {
	// DeleteExpired removes at most limit records that expired before now
	// and reports how many it removed.
	DeleteExpired(ctx context.Context, now time.Time, limit int) (int64, error)
}

// SweepBatch is how many records one statement removes. It bounds both the
// transaction and the locks it takes, so a sweep never blocks the write
// path it shares a table with.
const SweepBatch = 500

// maxBatchesPerSweep stops one pass from running unboundedly long after,
// say, a backlog builds up. What is left is simply removed by the next
// pass; nothing depends on a sweep finishing the table in one go.
const maxBatchesPerSweep = 200

// Collector removes expired idempotency records (SPECIFICATIONS.md section
// 24, point 5).
//
// Expiry is what makes a key reusable and what stops the table growing for
// ever. The request path already discards an expired record it happens to
// meet, but only for a key a client presents again; keys are usually unique
// per request, so without this job almost every record would live for ever.
//
// The job is safe to run on every replica: each pass deletes a bounded set
// of rows by primary key, so two replicas sweeping at once simply share the
// work.
type Collector struct {
	expirer  Expirer
	interval time.Duration
	logger   *slog.Logger
	now      func() time.Time
}

// CollectorOptions tune a Collector. Nil fields take safe defaults.
type CollectorOptions struct {
	Now func() time.Time
}

// NewCollector returns a collector that sweeps every interval.
func NewCollector(expirer Expirer, interval time.Duration, logger *slog.Logger, opts CollectorOptions) *Collector {
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	return &Collector{expirer: expirer, interval: interval, logger: logger, now: now}
}

// Run sweeps until ctx ends. It returns when the context is done, so the
// caller can wait for it during shutdown.
//
// A failed sweep is logged and retried at the next tick rather than
// escalated: retention falling behind is not a reason to stop serving.
func (c *Collector) Run(ctx context.Context) {
	ticker := time.NewTicker(c.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if _, err := c.Sweep(ctx); err != nil && ctx.Err() == nil {
				c.logger.WarnContext(ctx, "idempotency retention sweep failed", "error", err)
			}
		}
	}
}

// Sweep removes expired records in batches until none are left, and reports
// how many it removed. It stops early when the context ends, so shutdown is
// never delayed by a backlog.
func (c *Collector) Sweep(ctx context.Context) (int64, error) {
	var total int64
	for batch := 0; batch < maxBatchesPerSweep; batch++ {
		if err := ctx.Err(); err != nil {
			return total, err
		}
		removed, err := c.expirer.DeleteExpired(ctx, c.now(), SweepBatch)
		total += removed
		if err != nil {
			return total, err
		}
		if removed < SweepBatch {
			break
		}
	}
	if total > 0 {
		c.logger.InfoContext(ctx, "expired idempotency records removed", "count", total)
	}
	return total, nil
}
