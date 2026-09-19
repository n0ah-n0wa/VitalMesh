package postgres

import (
	"context"
	"time"
)

// Retention removes stored synthetic data that has outlived its window
// (SPECIFICATIONS.md section 81).
//
// Both statements delete by primary key, in bounded batches, so a sweep
// takes a small and predictable set of row locks on tables the write path
// is using at the same time. The caller repeats each one until it removes
// fewer rows than it asked for.
type Retention struct {
	db DB
}

// NewRetention returns a repository over db, which may be a pool or a
// transaction.
func NewRetention(db DB) *Retention { return &Retention{db: db} }

// DeleteMeasurementsBefore removes up to limit of the oldest readings
// recorded before cutoff and reports how many it removed.
//
// Nothing references a measurement: processing results are derived values
// that carry their own patient and type rather than a foreign key to the
// readings they came from, so removing a reading cannot orphan anything.
// That is what makes this safe to do without touching the rest of the
// graph, and it is checked by a test that deletes readings out from under
// a completed job and asserts the job and its results are still intact.
func (r *Retention) DeleteMeasurementsBefore(ctx context.Context, cutoff time.Time, limit int) (int64, error) {
	// id = ANY(ARRAY(...)) probes the primary key for the selected ids;
	// the IN (subquery) form plans as a hash semi-join over the whole
	// table, which is the opposite of what a bounded sweep wants.
	tag, err := r.db.Exec(ctx, `
		DELETE FROM measurements
		WHERE id = ANY(ARRAY(
			SELECT id FROM measurements
			WHERE recorded_at < $1
			ORDER BY recorded_at
			LIMIT $2
		))`, cutoff, limit)
	if err != nil {
		return 0, mapError(err)
	}
	return tag.RowsAffected(), nil
}

// DeleteResultsBefore removes up to limit of the oldest processing results
// created before cutoff and reports how many it removed.
//
// The job that produced a result is kept whatever happens to the result.
// Section 94 requires a failed job to preserve its diagnosis rather than
// disappear, and a job row is small and bounded by how many were ever
// requested; the results are what grow with the data.
func (r *Retention) DeleteResultsBefore(ctx context.Context, cutoff time.Time, limit int) (int64, error) {
	tag, err := r.db.Exec(ctx, `
		DELETE FROM processing_results
		WHERE id = ANY(ARRAY(
			SELECT id FROM processing_results
			WHERE created_at < $1
			ORDER BY created_at
			LIMIT $2
		))`, cutoff, limit)
	if err != nil {
		return 0, mapError(err)
	}
	return tag.RowsAffected(), nil
}
