package postgres

import (
	"context"
	"encoding/json"
	"fmt"
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
	// One statement for the whole batch (see insertMeasurementsMany). A job
	// of 60,000 readings over every window produces about 1,200 rows; the
	// baseline measured them as 1,200 statements.
	n := len(items)
	jobs := make([]string, n)
	types := make([]string, n)
	windows := make([]string, n)
	starts := make([]time.Time, n)
	statistics := make([]string, n)
	anomalies := make([]string, n)
	algorithms := make([]string, n)
	services := make([]string, n)
	for i, res := range items {
		jobs[i] = res.JobID.String()
		types[i] = string(res.MeasurementType)
		windows[i] = res.Window
		starts[i] = res.WindowStart
		statistics[i] = jsonOrEmpty(res.Statistics)
		anomalies[i] = jsonOrEmptyArray(res.Anomalies)
		algorithms[i] = res.AlgorithmVersion
		services[i] = res.ServiceVersion
	}
	rows, err := r.db.Query(ctx, `
		INSERT INTO processing_results
			(job_id, measurement_type, "window", window_start, statistics, anomalies, algorithm_version, service_version)
		SELECT * FROM unnest($1::uuid[], $2::text[], $3::text[], $4::timestamptz[], $5::jsonb[], $6::jsonb[], $7::text[], $8::text[])
		RETURNING `+resultColumns,
		jobs, types, windows, starts, statistics, anomalies, algorithms, services)
	stored, err := collectAll[domain.ProcessingResult](rows, err)
	if err != nil {
		return nil, err
	}
	// Back into input order by the result's key, unique within a job.
	type key struct {
		typ   domain.MeasurementType
		win   string
		start time.Time
	}
	byKey := make(map[key]int, n)
	for i, res := range items {
		byKey[key{res.MeasurementType, res.Window, res.WindowStart.UTC().Truncate(time.Microsecond)}] = i
	}
	ordered := make([]domain.ProcessingResult, n)
	placed := 0
	for _, res := range stored {
		i, ok := byKey[key{res.MeasurementType, res.Window, res.WindowStart.UTC().Truncate(time.Microsecond)}]
		if !ok {
			return nil, fmt.Errorf("stored result %s does not match any input", res.ID)
		}
		ordered[i] = res
		placed++
	}
	if placed != n || len(stored) != n {
		return nil, fmt.Errorf("stored %d results for %d inputs", len(stored), n)
	}
	return ordered, nil
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

// jsonOrEmpty is raw as text, or an empty object when nothing was given.
func jsonOrEmpty(raw json.RawMessage) string {
	if len(raw) == 0 {
		return "{}"
	}
	return string(raw)
}

// jsonOrEmptyArray is raw as text, or an empty array when nothing was given.
func jsonOrEmptyArray(raw json.RawMessage) string {
	if len(raw) == 0 {
		return "[]"
	}
	return string(raw)
}
