package postgres

import (
	"context"
	"encoding/json"
	"time"

	"github.com/google/uuid"

	"github.com/n0ah-n0wa/VitalMesh/services/api-gateway/internal/domain"
)

const jobColumns = `id, patient_id, status, parameters, algorithm_version, requested_at,
	started_at, completed_at, failed_at, cancelled_at, error_code, error_message, attempt_count,
	created_by, service_version, request_id, trace_id, lease_expires_at, created_at, updated_at`

// Jobs persists processing jobs. Every status change is a compare-and-set
// on the current status, and the database rejects transitions outside the
// state machine, so two writers cannot both move the same job.
type Jobs struct {
	db DB
}

// NewJobs returns a repository bound to db.
func NewJobs(db DB) *Jobs { return &Jobs{db: db} }

// NewJob is the input to Create.
type NewJob struct {
	PatientID        uuid.UUID
	Parameters       json.RawMessage
	AlgorithmVersion string
	CreatedBy        uuid.UUID
	RequestID        *string
	TraceID          *string
}

// JobCursor is the keyset position after which ListByPatient continues.
type JobCursor struct {
	CreatedAt time.Time `json:"created_at"`
	ID        uuid.UUID `json:"id"`
}

// Create inserts a PENDING job.
func (r *Jobs) Create(ctx context.Context, j NewJob) (domain.ProcessingJob, error) {
	rows, err := r.db.Query(ctx, `
		INSERT INTO processing_jobs (patient_id, parameters, algorithm_version, created_by, request_id, trace_id)
		VALUES ($1, $2, $3, $4, $5, $6)
		RETURNING `+jobColumns,
		j.PatientID, j.Parameters, j.AlgorithmVersion, j.CreatedBy, j.RequestID, j.TraceID)
	return collectOne[domain.ProcessingJob](rows, err)
}

// GetByID returns the job or a not-found error.
func (r *Jobs) GetByID(ctx context.Context, id uuid.UUID) (domain.ProcessingJob, error) {
	rows, err := r.db.Query(ctx, `SELECT `+jobColumns+` FROM processing_jobs WHERE id = $1`, id)
	return collectOne[domain.ProcessingJob](rows, err)
}

// ListByPatient returns up to limit jobs of one patient in creation order,
// starting after the cursor when one is given.
func (r *Jobs) ListByPatient(ctx context.Context, patientID uuid.UUID, after *JobCursor, limit int) ([]domain.ProcessingJob, error) {
	if after == nil {
		rows, err := r.db.Query(ctx, `
			SELECT `+jobColumns+` FROM processing_jobs
			WHERE patient_id = $1
			ORDER BY created_at, id
			LIMIT $2`, patientID, limit)
		return collectAll[domain.ProcessingJob](rows, err)
	}
	rows, err := r.db.Query(ctx, `
		SELECT `+jobColumns+` FROM processing_jobs
		WHERE patient_id = $1 AND (created_at, id) > ($2, $3)
		ORDER BY created_at, id
		LIMIT $4`, patientID, after.CreatedAt, after.ID, limit)
	return collectAll[domain.ProcessingJob](rows, err)
}

// ListPending returns the oldest PENDING jobs, up to limit, for dispatch.
func (r *Jobs) ListPending(ctx context.Context, limit int) ([]domain.ProcessingJob, error) {
	rows, err := r.db.Query(ctx, `
		SELECT `+jobColumns+` FROM processing_jobs
		WHERE status = 'PENDING'
		ORDER BY created_at, id
		LIMIT $1`, limit)
	return collectAll[domain.ProcessingJob](rows, err)
}

// Start moves a PENDING job to PROCESSING, counting the attempt and setting
// the lease. It fails with a conflict when the job is not PENDING.
func (r *Jobs) Start(ctx context.Context, id uuid.UUID, now, leaseExpiresAt time.Time, serviceVersion string) (domain.ProcessingJob, error) {
	rows, err := r.db.Query(ctx, `
		UPDATE processing_jobs
		SET status = 'PROCESSING', started_at = $2, lease_expires_at = $3,
		    attempt_count = attempt_count + 1, service_version = $4
		WHERE id = $1 AND status = 'PENDING'
		RETURNING `+jobColumns,
		id, now, leaseExpiresAt, serviceVersion)
	return r.transitioned(ctx, id, rows, err)
}

// Complete moves a PROCESSING job to COMPLETED.
func (r *Jobs) Complete(ctx context.Context, id uuid.UUID, now time.Time) (domain.ProcessingJob, error) {
	rows, err := r.db.Query(ctx, `
		UPDATE processing_jobs
		SET status = 'COMPLETED', completed_at = $2, lease_expires_at = NULL
		WHERE id = $1 AND status = 'PROCESSING'
		RETURNING `+jobColumns,
		id, now)
	return r.transitioned(ctx, id, rows, err)
}

// Fail moves a PROCESSING job to FAILED, recording a stable error code and
// a safe message.
func (r *Jobs) Fail(ctx context.Context, id uuid.UUID, now time.Time, code, message string) (domain.ProcessingJob, error) {
	rows, err := r.db.Query(ctx, `
		UPDATE processing_jobs
		SET status = 'FAILED', failed_at = $2, error_code = $3, error_message = $4, lease_expires_at = NULL
		WHERE id = $1 AND status = 'PROCESSING'
		RETURNING `+jobColumns,
		id, now, code, message)
	return r.transitioned(ctx, id, rows, err)
}

// ExpirePending moves PENDING jobs created before cutoff to PROCESSING with
// a lease that expired at now, counting the attempt, so that FailExpired
// can fail them: a job this old in PENDING was taken up by a request that
// died before dispatching it. At most limit rows; rows locked by another
// writer are skipped rather than waited for.
func (r *Jobs) ExpirePending(ctx context.Context, cutoff, now time.Time, limit int) (int64, error) {
	tag, err := r.db.Exec(ctx, `
		UPDATE processing_jobs
		SET status = 'PROCESSING', started_at = $2, lease_expires_at = $2, attempt_count = attempt_count + 1
		WHERE id IN (
			SELECT id FROM processing_jobs
			WHERE status = 'PENDING' AND created_at < $1
			ORDER BY created_at
			LIMIT $3
			FOR UPDATE SKIP LOCKED)`,
		cutoff, now, limit)
	if err != nil {
		return 0, mapError(err)
	}
	return tag.RowsAffected(), nil
}

// FailExpired moves PROCESSING jobs whose lease expired at or before now to
// FAILED with a stable error code and a safe message, at most limit of
// them, skipping rows another writer holds. A job whose lease is still
// valid is never touched.
func (r *Jobs) FailExpired(ctx context.Context, now time.Time, code, message string, limit int) (int64, error) {
	tag, err := r.db.Exec(ctx, `
		UPDATE processing_jobs
		SET status = 'FAILED', failed_at = $1, error_code = $2, error_message = $3, lease_expires_at = NULL
		WHERE id IN (
			SELECT id FROM processing_jobs
			WHERE status = 'PROCESSING' AND lease_expires_at <= $1
			ORDER BY lease_expires_at
			LIMIT $4
			FOR UPDATE SKIP LOCKED)`,
		now, code, message, limit)
	if err != nil {
		return 0, mapError(err)
	}
	return tag.RowsAffected(), nil
}

// Cancel moves a PENDING or PROCESSING job to CANCELLED.
func (r *Jobs) Cancel(ctx context.Context, id uuid.UUID, now time.Time) (domain.ProcessingJob, error) {
	rows, err := r.db.Query(ctx, `
		UPDATE processing_jobs
		SET status = 'CANCELLED', cancelled_at = $2, lease_expires_at = NULL
		WHERE id = $1 AND status IN ('PENDING', 'PROCESSING')
		RETURNING `+jobColumns,
		id, now)
	return r.transitioned(ctx, id, rows, err)
}

// transitioned resolves a conditional update that matched no row into
// not-found or conflict.
func (r *Jobs) transitioned(ctx context.Context, id uuid.UUID, rows rowsResult, err error) (domain.ProcessingJob, error) {
	job, err := collectOne[domain.ProcessingJob](rows, err)
	if err == nil {
		return job, nil
	}
	var domErr *domain.Error
	if !asDomain(err, &domErr) || domErr.Kind != domain.KindNotFound {
		return domain.ProcessingJob{}, err
	}
	current, getErr := r.GetByID(ctx, id)
	if getErr != nil {
		return domain.ProcessingJob{}, getErr
	}
	return domain.ProcessingJob{}, domain.New(domain.KindConflict, "INVALID_JOB_TRANSITION",
		"The job is "+string(current.Status)+" and cannot make this transition.")
}
