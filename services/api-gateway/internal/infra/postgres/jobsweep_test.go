//go:build integration

package postgres_test

import (
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/n0ah-n0wa/VitalMesh/services/api-gateway/internal/domain"
	"github.com/n0ah-n0wa/VitalMesh/services/api-gateway/internal/infra/postgres"
	"github.com/n0ah-n0wa/VitalMesh/services/api-gateway/internal/processing"
)

// The lease sweep (SPECIFICATIONS.md sections 37, 92 and 94), verified
// against the database that enforces the state machine: a job left
// PROCESSING by a gateway that stopped is failed once its lease expires, a
// job left PENDING by a request that died before dispatching it is started
// and failed the way an abandoned dispatch is, and nothing with a valid
// lease or a terminal state is touched.
func TestFailExpiredJobsReclaimsInterruptedJobsAndNothingElse(t *testing.T) {
	f := newStateFixture(t)
	store := postgres.NewJobStore(f.pool, postgres.JobStoreOptions{Lease: time.Minute})
	now := time.Now().UTC().Truncate(time.Microsecond)
	start := func(id uuid.UUID, at time.Time) {
		t.Helper()
		if _, err := store.StartJob(f.ctx, id, at, "api-gateway"); err != nil {
			t.Fatalf("start %s: %v", id, err)
		}
	}
	// age moves a job's request and creation back in time, as the schema
	// requires a start no earlier than the request.
	age := func(id uuid.UUID, at time.Time) {
		t.Helper()
		if _, err := f.pool.Exec(f.ctx, `UPDATE processing_jobs SET requested_at = $2, created_at = $2 WHERE id = $1`, id, at); err != nil {
			t.Fatalf("age %s: %v", id, err)
		}
	}
	get := func(id uuid.UUID) domain.ProcessingJob {
		t.Helper()
		job, err := f.jobs.GetByID(f.ctx, id)
		if err != nil {
			t.Fatalf("get %s: %v", id, err)
		}
		return job
	}

	// Interrupted: started two minutes ago under a one-minute lease.
	interrupted := f.pending(t)
	age(interrupted.ID, now.Add(-3*time.Minute))
	start(interrupted.ID, now.Add(-2*time.Minute))
	// Live: requested and started just now, its lease valid.
	live := f.pending(t)
	age(live.ID, now)
	start(live.ID, now)
	// Orphaned: created two minutes ago by a request that died before it
	// could dispatch the job.
	orphan := f.pending(t)
	age(orphan.ID, now.Add(-2*time.Minute))
	// Terminal: completed long ago, and a lease is never held by a
	// terminal job.
	done := f.pending(t)
	age(done.ID, now.Add(-4*time.Minute))
	start(done.ID, now.Add(-3*time.Minute))
	if _, err := f.jobs.Complete(f.ctx, done.ID, now.Add(-2*time.Minute)); err != nil {
		t.Fatalf("complete: %v", err)
	}

	failed, err := store.FailExpiredJobs(f.ctx, now, processing.InterruptedFailure(), processing.SweepBatch)
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if failed != 2 {
		t.Errorf("the sweep failed %d jobs, want 2: the interrupted one and the orphaned one", failed)
	}

	for _, c := range []struct {
		name string
		id   uuid.UUID
	}{{"interrupted", interrupted.ID}, {"orphaned", orphan.ID}} {
		job := get(c.id)
		if job.Status != domain.JobFailed {
			t.Errorf("the %s job is %s, want FAILED", c.name, job.Status)
		}
		if job.ErrorCode == nil || *job.ErrorCode != processing.CodeProcessingInterrupted {
			t.Errorf("the %s job carries code %v, want %s", c.name, job.ErrorCode, processing.CodeProcessingInterrupted)
		}
		if job.ErrorMessage == nil || *job.ErrorMessage == "" {
			t.Errorf("the %s job has no error message", c.name)
		}
		if job.FailedAt == nil || !job.FailedAt.Equal(now) {
			t.Errorf("the %s job failed_at = %v, want the sweep's clock %s", c.name, job.FailedAt, now)
		}
		if job.StartedAt == nil {
			t.Errorf("the %s job has no started_at, which a FAILED job must have", c.name)
		}
		if job.AttemptCount != 1 {
			t.Errorf("the %s job attempt_count = %d, want 1", c.name, job.AttemptCount)
		}
		if job.LeaseExpiresAt != nil {
			t.Errorf("the %s job still holds a lease after failing", c.name)
		}
	}
	if job := get(live.ID); job.Status != domain.JobProcessing || job.LeaseExpiresAt == nil {
		t.Errorf("the live job = %s (lease %v), want PROCESSING with its lease intact", job.Status, job.LeaseExpiresAt)
	}
	if job := get(done.ID); job.Status != domain.JobCompleted {
		t.Errorf("the completed job = %s, want COMPLETED", job.Status)
	}

	// A second pass finds nothing: the sweep is idempotent.
	if again, err := store.FailExpiredJobs(f.ctx, now, processing.InterruptedFailure(), processing.SweepBatch); err != nil || again != 0 {
		t.Errorf("a second sweep failed %d jobs (err %v), want 0", again, err)
	}

	// A pass is bounded by its limit, and what it leaves is taken next.
	for i := 0; i < 3; i++ {
		stale := f.pending(t)
		age(stale.ID, now.Add(-3*time.Minute))
		start(stale.ID, now.Add(-2*time.Minute))
	}
	if n, err := store.FailExpiredJobs(f.ctx, now, processing.InterruptedFailure(), 2); err != nil || n != 2 {
		t.Errorf("a sweep limited to 2 failed %d (err %v)", n, err)
	}
	if n, err := store.FailExpiredJobs(f.ctx, now, processing.InterruptedFailure(), 2); err != nil || n != 1 {
		t.Errorf("the next sweep failed %d (err %v), want the 1 left over", n, err)
	}
}
