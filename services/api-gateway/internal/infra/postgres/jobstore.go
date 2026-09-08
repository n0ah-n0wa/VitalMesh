package postgres

import (
	"context"
	"encoding/json"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/n0ah-n0wa/VitalMesh/services/api-gateway/internal/domain"
	"github.com/n0ah-n0wa/VitalMesh/services/api-gateway/internal/processing"
)

// JobStore implements processing.Store.
//
// Every method is one transaction or one read, and none of them calls
// anything outside the database. The external call the Processing API makes
// happens between two of these methods, never inside one, so no transaction
// is ever open while the processor is being waited on (SPECIFICATIONS.md
// section 22).
type JobStore struct {
	pool *pgxpool.Pool
}

// NewJobStore returns a store over pool.
func NewJobStore(pool *pgxpool.Pool) *JobStore {
	return &JobStore{pool: pool}
}

// CreateJob inserts a PENDING job and appends its audit record atomically,
// so a job never exists without the record of who asked for it.
func (s *JobStore) CreateJob(ctx context.Context, in processing.NewJob, event processing.AuditEvent) (domain.ProcessingJob, error) {
	parameters, err := json.Marshal(in.Parameters)
	if err != nil {
		return domain.ProcessingJob{}, err
	}
	var created domain.ProcessingJob
	err = WithTx(ctx, s.pool, func(tx pgx.Tx) error {
		requestID := optional(in.RequestID)
		correlationID := optional(in.CorrelationID)
		created, err = NewJobs(tx).Create(ctx, NewJob{
			PatientID:        in.PatientID,
			Parameters:       parameters,
			AlgorithmVersion: in.AlgorithmVersion,
			CreatedBy:        in.CreatedBy,
			RequestID:        requestID,
			// The correlation id is stored in trace_id: it is what ties this
			// job to the client request that caused it, across services.
			TraceID: correlationID,
		})
		if err != nil {
			return err
		}
		actor := event.ActorID
		jobID := created.ID
		metadata, err := json.Marshal(jobAuditMetadata{
			PatientID:        created.PatientID,
			AlgorithmVersion: created.AlgorithmVersion,
			Parameters:       in.Parameters,
		})
		if err != nil {
			return err
		}
		_, err = NewAudit(tx).Append(ctx, NewAuditEntry{
			ActorID:      &actor,
			ActorType:    domain.ActorUser,
			Action:       event.Action,
			ResourceType: processing.ResourceType,
			ResourceID:   &jobID,
			RequestID:    event.RequestID,
			Metadata:     metadata,
		})
		return err
	})
	if err != nil {
		return domain.ProcessingJob{}, err
	}
	return created, nil
}

// jobAuditMetadata is the safe context stored with a job's audit record:
// what was asked for, never any measurement.
type jobAuditMetadata struct {
	PatientID        uuid.UUID             `json:"patient_id"`
	AlgorithmVersion string                `json:"algorithm_version"`
	Parameters       processing.Parameters `json:"parameters"`
}

// StartJob moves a PENDING job to PROCESSING. It is a compare-and-set on
// the status, so two gateways racing on the same job cannot both start it.
func (s *JobStore) StartJob(ctx context.Context, id uuid.UUID, now time.Time, serviceVersion string) (domain.ProcessingJob, error) {
	// The lease says how long this attempt may hold the job before another
	// worker could take it over. It is the caller's own bound; nothing
	// reclaims expired leases yet, and the column records the intent.
	lease := now.Add(jobLease)
	return NewJobs(s.pool).Start(ctx, id, now, lease, serviceVersion)
}

// jobLease is how long a dispatch may hold a job.
const jobLease = 15 * time.Minute

// CompleteJob stores the results and moves the job to COMPLETED in one
// transaction. Either both happen or neither does, so a COMPLETED job
// always has its results and results never belong to an unfinished job.
func (s *JobStore) CompleteJob(ctx context.Context, id uuid.UUID, now time.Time, results []processing.StoredResult, serviceVersion string) (domain.ProcessingJob, error) {
	rows := make([]NewResult, len(results))
	for i, r := range results {
		rows[i] = NewResult{
			JobID:            id,
			MeasurementType:  r.MeasurementType,
			Window:           r.Window,
			WindowStart:      r.WindowStart,
			Statistics:       r.Statistics,
			Anomalies:        r.Anomalies,
			AlgorithmVersion: r.AlgorithmVersion,
			ServiceVersion:   r.ServiceVersion,
		}
	}
	var completed domain.ProcessingJob
	err := WithTx(ctx, s.pool, func(tx pgx.Tx) error {
		if len(rows) > 0 {
			if _, err := NewResults(tx).CreateBatch(ctx, rows); err != nil {
				return err
			}
		}
		var err error
		completed, err = NewJobs(tx).Complete(ctx, id, now)
		if err != nil {
			return err
		}
		if serviceVersion != "" {
			return setJobServiceVersion(ctx, tx, id, serviceVersion)
		}
		return nil
	})
	if err != nil {
		return domain.ProcessingJob{}, err
	}
	if serviceVersion != "" {
		completed.ServiceVersion = &serviceVersion
	}
	return completed, nil
}

// setJobServiceVersion records which processor build produced the results.
func setJobServiceVersion(ctx context.Context, tx pgx.Tx, id uuid.UUID, version string) error {
	_, err := tx.Exec(ctx, `UPDATE processing_jobs SET service_version = $2 WHERE id = $1`, id, version)
	if err != nil {
		return mapError(err)
	}
	return nil
}

// FailJob moves a PROCESSING job to FAILED with its diagnosis. A failed job
// keeps its record (SPECIFICATIONS.md section 94).
func (s *JobStore) FailJob(ctx context.Context, id uuid.UUID, now time.Time, failure processing.Failure) (domain.ProcessingJob, error) {
	return NewJobs(s.pool).Fail(ctx, id, now, failure.Code, failure.Message)
}

// GetJob returns the job whatever its status.
func (s *JobStore) GetJob(ctx context.Context, id uuid.UUID) (domain.ProcessingJob, error) {
	return NewJobs(s.pool).GetByID(ctx, id)
}

// ReadingsForJob returns the readings a job is to process: those of the
// patient, of the requested types, inside the requested range, in recording
// order. It reads at most limit rows, which the caller sets one above the
// job bound so an oversized range is detected without a second query.
//
// The reads run in one read-only snapshot. A job asks for several
// measurement types and each is a separate query, so without a snapshot a
// write landing between two of them would put the job's types at different
// instants and the same job run twice could see different data. The
// snapshot takes no locks and holds no external call, so it does not
// conflict with the rule that no transaction spans the processor call.
func (s *JobStore) ReadingsForJob(ctx context.Context, patientID uuid.UUID, params processing.Parameters, limit int) ([]domain.Measurement, error) {
	if len(params.MeasurementTypes) == 0 {
		return nil, nil
	}
	out := make([]domain.Measurement, 0, min(limit, 1024))
	err := WithSnapshot(ctx, s.pool, func(tx pgx.Tx) error {
		repo := NewMeasurements(tx)
		// One query per requested type keeps every read on the
		// (patient_id, type, recorded_at) index; the types are few and
		// bounded by the catalogue.
		for _, t := range params.MeasurementTypes {
			if len(out) >= limit {
				break
			}
			typ := t
			page, err := repo.ListByPatient(ctx, patientID, MeasurementFilter{
				Type: &typ,
				From: params.From,
				To:   params.To,
			}, nil, limit-len(out))
			if err != nil {
				return err
			}
			out = append(out, page...)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// ListResultsByPatient returns a page of a patient's results.
func (s *JobStore) ListResultsByPatient(ctx context.Context, patientID uuid.UUID, after *processing.ResultCursor, limit int) ([]domain.ProcessingResult, error) {
	var cursor *ResultCursor
	if after != nil {
		cursor = &ResultCursor{
			MeasurementType: after.MeasurementType,
			Window:          after.Window,
			WindowStart:     after.WindowStart,
			JobID:           after.JobID,
		}
	}
	return NewResults(s.pool).ListByPatient(ctx, patientID, cursor, limit)
}

// PatientStatus returns the status of a patient, or a not-found error.
func (s *JobStore) PatientStatus(ctx context.Context, id uuid.UUID) (domain.PatientStatus, error) {
	p, err := NewPatients(s.pool).GetByID(ctx, id)
	if err != nil {
		return "", err
	}
	return p.Status, nil
}

func optional(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}
