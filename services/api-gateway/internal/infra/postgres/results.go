package postgres

import (
	"context"
	"encoding/json"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/n0ah-n0wa/VitalMesh/services/api-gateway/internal/domain"
)

// rowsResult names the pgx rows type where it is passed through helpers.
type rowsResult = pgx.Rows

const resultColumns = `id, job_id, patient_id, measurement_type, "window", window_start, statistics, anomalies,
	algorithm_version, service_version, created_at`

// patient_id is set by the database from the job; it is never supplied.
const insertResult = `
	INSERT INTO processing_results
		(job_id, measurement_type, "window", window_start, statistics, anomalies, algorithm_version, service_version)
	VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
	RETURNING ` + resultColumns

// Results persists processing results.
type Results struct {
	db DB
}

// NewResults returns a repository bound to db.
func NewResults(db DB) *Results { return &Results{db: db} }

// NewResult is the input to Create and CreateBatch.
type NewResult struct {
	JobID            uuid.UUID
	MeasurementType  domain.MeasurementType
	Window           string
	WindowStart      time.Time
	Statistics       json.RawMessage
	Anomalies        json.RawMessage
	AlgorithmVersion string
	ServiceVersion   string
}

func (res NewResult) args() []any {
	anomalies := res.Anomalies
	if len(anomalies) == 0 {
		anomalies = json.RawMessage(`[]`)
	}
	return []any{res.JobID, res.MeasurementType, res.Window, res.WindowStart, res.Statistics, anomalies,
		res.AlgorithmVersion, res.ServiceVersion}
}

// ResultCursor is the keyset position after which ListByJob and
// ListByPatient continue: type, window, window start and, for the patient
// scope where two jobs can produce the same window, the job as tie-break.
// Both orders are covered by an index, so every page is one index range
// scan whatever the collection size.
type ResultCursor struct {
	MeasurementType domain.MeasurementType `json:"measurement_type"`
	Window          string                 `json:"window"`
	WindowStart     time.Time              `json:"window_start"`
	JobID           uuid.UUID              `json:"job_id"`
}

// CursorAfter returns the cursor that continues after res.
func CursorAfter(res domain.ProcessingResult) ResultCursor {
	return ResultCursor{MeasurementType: res.MeasurementType, Window: res.Window, WindowStart: res.WindowStart, JobID: res.JobID}
}

// Create inserts a result. A second result for the same job, type and
// window instance is a conflict.
func (r *Results) Create(ctx context.Context, res NewResult) (domain.ProcessingResult, error) {
	rows, err := r.db.Query(ctx, insertResult, res.args()...)
	return collectOne[domain.ProcessingResult](rows, err)
}

// CreateBatch inserts a job's results in one round trip, all or none, and
// returns them in input order.
func (r *Results) CreateBatch(ctx context.Context, items []NewResult) ([]domain.ProcessingResult, error) {
	if len(items) == 0 {
		return []domain.ProcessingResult{}, nil
	}
	batch := &pgx.Batch{}
	for _, res := range items {
		batch.Queue(insertResult, res.args()...)
	}
	return collectBatch[domain.ProcessingResult](r.db.SendBatch(ctx, batch), len(items))
}

// ListByJob returns up to limit results of a job ordered by type, window
// and window start, starting after the cursor when one is given. The
// cursor's JobID is ignored: the job is fixed.
func (r *Results) ListByJob(ctx context.Context, jobID uuid.UUID, after *ResultCursor, limit int) ([]domain.ProcessingResult, error) {
	if after == nil {
		rows, err := r.db.Query(ctx, `
			SELECT `+resultColumns+` FROM processing_results
			WHERE job_id = $1
			ORDER BY measurement_type, "window", window_start
			LIMIT $2`, jobID, limit)
		return collectAll[domain.ProcessingResult](rows, err)
	}
	rows, err := r.db.Query(ctx, `
		SELECT `+resultColumns+` FROM processing_results
		WHERE job_id = $1 AND (measurement_type, "window", window_start) > ($2, $3, $4)
		ORDER BY measurement_type, "window", window_start
		LIMIT $5`, jobID, after.MeasurementType, after.Window, after.WindowStart, limit)
	return collectAll[domain.ProcessingResult](rows, err)
}

// ListByPatient returns up to limit results across all of a patient's jobs
// ordered by type, window, window start and job, starting after the cursor
// when one is given.
func (r *Results) ListByPatient(ctx context.Context, patientID uuid.UUID, after *ResultCursor, limit int) ([]domain.ProcessingResult, error) {
	if after == nil {
		rows, err := r.db.Query(ctx, `
			SELECT `+resultColumns+` FROM processing_results
			WHERE patient_id = $1
			ORDER BY measurement_type, "window", window_start, job_id
			LIMIT $2`, patientID, limit)
		return collectAll[domain.ProcessingResult](rows, err)
	}
	rows, err := r.db.Query(ctx, `
		SELECT `+resultColumns+` FROM processing_results
		WHERE patient_id = $1
		  AND (measurement_type, "window", window_start, job_id) > ($2, $3, $4, $5)
		ORDER BY measurement_type, "window", window_start, job_id
		LIMIT $6`, patientID, after.MeasurementType, after.Window, after.WindowStart, after.JobID, limit)
	return collectAll[domain.ProcessingResult](rows, err)
}
