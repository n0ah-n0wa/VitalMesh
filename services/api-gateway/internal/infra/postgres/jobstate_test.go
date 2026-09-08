//go:build integration

package postgres_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/n0ah-n0wa/VitalMesh/services/api-gateway/internal/domain"
	"github.com/n0ah-n0wa/VitalMesh/services/api-gateway/internal/infra/postgres"
	"github.com/n0ah-n0wa/VitalMesh/services/api-gateway/internal/infra/postgres/postgrestest"
)

// The job state machine of SPECIFICATIONS.md section 92, verified against
// the database that enforces it:
//
//	PENDING    -> PROCESSING
//	PENDING    -> CANCELLED
//	PROCESSING -> COMPLETED
//	PROCESSING -> FAILED
//	PROCESSING -> CANCELLED
//
// Every other transition must be rejected. The rule lives in a trigger, so
// it holds for every writer, not only for the ones that go through this
// package.

type stateFixture struct {
	jobs      *postgres.Jobs
	patientID uuid.UUID
	userID    uuid.UUID
	ctx       context.Context
}

func newStateFixture(t *testing.T) *stateFixture {
	t.Helper()
	pool, _, _ := postgrestest.New(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)

	user, err := postgres.NewUsers(pool).Create(ctx, "state@example.com", "$argon2id$v=19$m=8,t=1,p=1$c2FsdHNhbHQ$aGFzaGhhc2hoYXNo", domain.RoleOperator)
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	patient, err := postgres.NewPatients(pool).Create(ctx, "state-patient", time.Date(1980, 1, 1, 0, 0, 0, 0, time.UTC), domain.SexUnknown)
	if err != nil {
		t.Fatalf("create patient: %v", err)
	}
	return &stateFixture{jobs: postgres.NewJobs(pool), patientID: patient.ID, userID: user.ID, ctx: ctx}
}

// pending inserts a fresh job in PENDING.
func (f *stateFixture) pending(t *testing.T) domain.ProcessingJob {
	t.Helper()
	job, err := f.jobs.Create(f.ctx, postgres.NewJob{
		PatientID:        f.patientID,
		Parameters:       json.RawMessage(`{"measurement_types":["HEART_RATE"],"windows":["1h"]}`),
		AlgorithmVersion: "1.0.0",
		CreatedBy:        f.userID,
	})
	if err != nil {
		t.Fatalf("create job: %v", err)
	}
	if job.Status != domain.JobPending {
		t.Fatalf("a new job is %s, want PENDING", job.Status)
	}
	return job
}

// processing inserts a job and moves it to PROCESSING.
func (f *stateFixture) processing(t *testing.T) domain.ProcessingJob {
	t.Helper()
	job := f.pending(t)
	started, err := f.jobs.Start(f.ctx, job.ID, later(), later(), "v-test")
	if err != nil {
		t.Fatalf("start job: %v", err)
	}
	return started
}

// later is a transition instant safely after any requested_at the database
// has just stamped, which the monotonicity constraint requires.
func later() time.Time { return time.Now().UTC().Add(time.Minute) }

func TestEveryValidTransitionIsAccepted(t *testing.T) {
	f := newStateFixture(t)

	t.Run("PENDING to PROCESSING", func(t *testing.T) {
		job := f.pending(t)
		started, err := f.jobs.Start(f.ctx, job.ID, later(), later(), "v-test")
		if err != nil {
			t.Fatalf("Start: %v", err)
		}
		if started.Status != domain.JobProcessing {
			t.Errorf("status = %s", started.Status)
		}
		if started.StartedAt == nil {
			t.Error("started_at must be set")
		}
		if started.AttemptCount != 1 {
			t.Errorf("attempt_count = %d, want the attempt counted", started.AttemptCount)
		}
	})

	t.Run("PENDING to CANCELLED", func(t *testing.T) {
		job := f.pending(t)
		cancelled, err := f.jobs.Cancel(f.ctx, job.ID, later())
		if err != nil {
			t.Fatalf("Cancel: %v", err)
		}
		if cancelled.Status != domain.JobCancelled {
			t.Errorf("status = %s", cancelled.Status)
		}
		if cancelled.CancelledAt == nil {
			t.Error("cancelled_at must be set")
		}
		if cancelled.StartedAt != nil {
			t.Error("a job cancelled before it ran never started")
		}
	})

	t.Run("PROCESSING to COMPLETED", func(t *testing.T) {
		job := f.processing(t)
		done, err := f.jobs.Complete(f.ctx, job.ID, later())
		if err != nil {
			t.Fatalf("Complete: %v", err)
		}
		if done.Status != domain.JobCompleted || done.CompletedAt == nil {
			t.Errorf("job = %+v", done)
		}
		if done.LeaseExpiresAt != nil {
			t.Error("a finished job holds no lease")
		}
	})

	t.Run("PROCESSING to FAILED", func(t *testing.T) {
		job := f.processing(t)
		failed, err := f.jobs.Fail(f.ctx, job.ID, later(), "PROCESSOR_UNAVAILABLE", "The processing service is unavailable.")
		if err != nil {
			t.Fatalf("Fail: %v", err)
		}
		if failed.Status != domain.JobFailed || failed.FailedAt == nil {
			t.Errorf("job = %+v", failed)
		}
		if failed.ErrorCode == nil || *failed.ErrorCode != "PROCESSOR_UNAVAILABLE" {
			t.Errorf("error_code = %v, want the diagnosis kept", failed.ErrorCode)
		}
	})

	t.Run("PROCESSING to CANCELLED", func(t *testing.T) {
		job := f.processing(t)
		cancelled, err := f.jobs.Cancel(f.ctx, job.ID, later())
		if err != nil {
			t.Fatalf("Cancel: %v", err)
		}
		if cancelled.Status != domain.JobCancelled || cancelled.CancelledAt == nil {
			t.Errorf("job = %+v", cancelled)
		}
	})
}

// Every transition the state machine does not name must be refused, and the
// job must be left exactly as it was.
func TestEveryInvalidTransitionIsRejected(t *testing.T) {
	f := newStateFixture(t)

	// terminal builds a job already in the given terminal state.
	terminal := func(t *testing.T, status domain.JobStatus) domain.ProcessingJob {
		t.Helper()
		switch status {
		case domain.JobCompleted:
			job := f.processing(t)
			done, err := f.jobs.Complete(f.ctx, job.ID, later())
			if err != nil {
				t.Fatalf("complete: %v", err)
			}
			return done
		case domain.JobFailed:
			job := f.processing(t)
			failed, err := f.jobs.Fail(f.ctx, job.ID, later(), "CODE", "message")
			if err != nil {
				t.Fatalf("fail: %v", err)
			}
			return failed
		case domain.JobCancelled:
			job := f.pending(t)
			cancelled, err := f.jobs.Cancel(f.ctx, job.ID, later())
			if err != nil {
				t.Fatalf("cancel: %v", err)
			}
			return cancelled
		}
		t.Fatalf("not a terminal state: %s", status)
		return domain.ProcessingJob{}
	}

	cases := []struct {
		name string
		// from prepares a job in the state under test.
		from func(*testing.T) domain.ProcessingJob
		// attempt tries the transition that must be refused.
		attempt func(uuid.UUID) (domain.ProcessingJob, error)
	}{
		{"PENDING to COMPLETED", func(t *testing.T) domain.ProcessingJob { return f.pending(t) },
			func(id uuid.UUID) (domain.ProcessingJob, error) { return f.jobs.Complete(f.ctx, id, later()) }},
		{"PENDING to FAILED", func(t *testing.T) domain.ProcessingJob { return f.pending(t) },
			func(id uuid.UUID) (domain.ProcessingJob, error) { return f.jobs.Fail(f.ctx, id, later(), "C", "m") }},

		{"PROCESSING to PROCESSING", func(t *testing.T) domain.ProcessingJob { return f.processing(t) },
			func(id uuid.UUID) (domain.ProcessingJob, error) {
				return f.jobs.Start(f.ctx, id, later(), later(), "v")
			}},

		{"COMPLETED to PROCESSING", func(t *testing.T) domain.ProcessingJob { return terminal(t, domain.JobCompleted) },
			func(id uuid.UUID) (domain.ProcessingJob, error) {
				return f.jobs.Start(f.ctx, id, later(), later(), "v")
			}},
		{"COMPLETED to FAILED", func(t *testing.T) domain.ProcessingJob { return terminal(t, domain.JobCompleted) },
			func(id uuid.UUID) (domain.ProcessingJob, error) { return f.jobs.Fail(f.ctx, id, later(), "C", "m") }},
		{"COMPLETED to CANCELLED", func(t *testing.T) domain.ProcessingJob { return terminal(t, domain.JobCompleted) },
			func(id uuid.UUID) (domain.ProcessingJob, error) { return f.jobs.Cancel(f.ctx, id, later()) }},

		{"FAILED to PROCESSING", func(t *testing.T) domain.ProcessingJob { return terminal(t, domain.JobFailed) },
			func(id uuid.UUID) (domain.ProcessingJob, error) {
				return f.jobs.Start(f.ctx, id, later(), later(), "v")
			}},
		{"FAILED to COMPLETED", func(t *testing.T) domain.ProcessingJob { return terminal(t, domain.JobFailed) },
			func(id uuid.UUID) (domain.ProcessingJob, error) { return f.jobs.Complete(f.ctx, id, later()) }},
		{"FAILED to CANCELLED", func(t *testing.T) domain.ProcessingJob { return terminal(t, domain.JobFailed) },
			func(id uuid.UUID) (domain.ProcessingJob, error) { return f.jobs.Cancel(f.ctx, id, later()) }},

		{"CANCELLED to PROCESSING", func(t *testing.T) domain.ProcessingJob { return terminal(t, domain.JobCancelled) },
			func(id uuid.UUID) (domain.ProcessingJob, error) {
				return f.jobs.Start(f.ctx, id, later(), later(), "v")
			}},
		{"CANCELLED to COMPLETED", func(t *testing.T) domain.ProcessingJob { return terminal(t, domain.JobCancelled) },
			func(id uuid.UUID) (domain.ProcessingJob, error) { return f.jobs.Complete(f.ctx, id, later()) }},
		{"CANCELLED to FAILED", func(t *testing.T) domain.ProcessingJob { return terminal(t, domain.JobCancelled) },
			func(id uuid.UUID) (domain.ProcessingJob, error) { return f.jobs.Fail(f.ctx, id, later(), "C", "m") }},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			before := tc.from(t)

			_, err := tc.attempt(before.ID)
			if err == nil {
				t.Fatalf("%s was accepted; the state machine forbids it", tc.name)
			}
			var domErr *domain.Error
			if !errors.As(err, &domErr) {
				t.Fatalf("err = %v, want a classified error", err)
			}
			if domErr.Kind != domain.KindConflict {
				t.Errorf("kind = %s, want conflict", domErr.Kind)
			}
			if domErr.Code != "INVALID_JOB_TRANSITION" {
				t.Errorf("code = %s", domErr.Code)
			}

			after, err := f.jobs.GetByID(f.ctx, before.ID)
			if err != nil {
				t.Fatalf("GetByID: %v", err)
			}
			if after.Status != before.Status {
				t.Errorf("the refused transition changed the status from %s to %s", before.Status, after.Status)
			}
			if after.AttemptCount != before.AttemptCount {
				t.Errorf("the refused transition changed attempt_count from %d to %d",
					before.AttemptCount, after.AttemptCount)
			}
		})
	}
}

// The trigger, not the WHERE clause, is what makes the rule hold for every
// writer. This drives a transition straight through SQL, bypassing the
// repository's compare-and-set, and it must still be refused.
func TestTheDatabaseItselfRefusesAnInvalidTransition(t *testing.T) {
	pool, _, _ := postgrestest.New(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	f := &stateFixture{jobs: postgres.NewJobs(pool), ctx: ctx}
	user, err := postgres.NewUsers(pool).Create(ctx, "trigger@example.com", "$argon2id$v=19$m=8,t=1,p=1$c2FsdHNhbHQ$aGFzaGhhc2hoYXNo", domain.RoleOperator)
	if err != nil {
		t.Fatal(err)
	}
	patient, err := postgres.NewPatients(pool).Create(ctx, "trigger-patient", time.Date(1980, 1, 1, 0, 0, 0, 0, time.UTC), domain.SexUnknown)
	if err != nil {
		t.Fatal(err)
	}
	f.userID, f.patientID = user.ID, patient.ID

	job := f.pending(t)
	// PENDING straight to COMPLETED, with every column the constraints want.
	_, err = pool.Exec(ctx, `
		UPDATE processing_jobs
		SET status = 'COMPLETED', started_at = $2, completed_at = $2
		WHERE id = $1`, job.ID, later())
	if err == nil {
		t.Fatal("the database accepted PENDING -> COMPLETED written directly")
	}

	after, err := f.jobs.GetByID(ctx, job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.Status != domain.JobPending {
		t.Errorf("status = %s, want the job untouched", after.Status)
	}
}

// A job that has already been started must not be started again, however
// many callers try at once: the compare-and-set means exactly one wins.
func TestOnlyOneCallerCanStartAJob(t *testing.T) {
	f := newStateFixture(t)
	job := f.pending(t)

	type result struct {
		err error
	}
	const racers = 8
	results := make(chan result, racers)
	for i := 0; i < racers; i++ {
		go func() {
			_, err := f.jobs.Start(f.ctx, job.ID, later(), later(), "v-race")
			results <- result{err}
		}()
	}

	won := 0
	for i := 0; i < racers; i++ {
		if r := <-results; r.err == nil {
			won++
		}
	}
	if won != 1 {
		t.Errorf("%d callers started the same job, want exactly 1", won)
	}

	after, err := f.jobs.GetByID(f.ctx, job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.AttemptCount != 1 {
		t.Errorf("attempt_count = %d, want exactly one counted attempt", after.AttemptCount)
	}
}
