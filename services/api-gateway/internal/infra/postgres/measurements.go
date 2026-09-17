package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/n0ah-n0wa/VitalMesh/services/api-gateway/internal/domain"
	"github.com/n0ah-n0wa/VitalMesh/services/api-gateway/internal/processing"
)

const measurementColumns = `id, patient_id, type, value, unit, recorded_at, created_at, source, metadata`

const insertMeasurement = `
	INSERT INTO measurements (patient_id, type, value, unit, recorded_at, source, metadata)
	VALUES ($1, $2, $3, $4, $5, $6, $7)
	RETURNING ` + measurementColumns

// Measurements persists readings.
type Measurements struct {
	db DB
}

// NewMeasurements returns a repository bound to db.
func NewMeasurements(db DB) *Measurements { return &Measurements{db: db} }

// NewMeasurement is the input to Create and CreateBatch.
type NewMeasurement struct {
	PatientID  uuid.UUID
	Type       domain.MeasurementType
	Value      float64
	Unit       string
	RecordedAt time.Time
	Source     string
	// Metadata must be a JSON object; nil means an empty object.
	Metadata json.RawMessage
}

func (m NewMeasurement) args() []any {
	metadata := m.Metadata
	if len(metadata) == 0 {
		metadata = json.RawMessage(`{}`)
	}
	return []any{m.PatientID, m.Type, m.Value, m.Unit, m.RecordedAt, m.Source, metadata}
}

// MeasurementCursor is the keyset position after which ListByPatient continues.
type MeasurementCursor struct {
	RecordedAt time.Time `json:"recorded_at"`
	ID         uuid.UUID `json:"id"`
}

// MeasurementFilter narrows ListByPatient. Nil fields are ignored. The time
// range is half-open: From inclusive, To exclusive.
type MeasurementFilter struct {
	Type *domain.MeasurementType
	From *time.Time
	To   *time.Time
}

// Create inserts a reading. The database rejects unknown types, unit
// mismatches, out-of-range values and duplicates.
func (r *Measurements) Create(ctx context.Context, m NewMeasurement) (domain.Measurement, error) {
	rows, err := r.db.Query(ctx, insertMeasurement, m.args()...)
	return collectOne[domain.Measurement](rows, err)
}

// insertMeasurementsMany stores a whole batch as one statement: the rows
// arrive as parallel arrays and unnest turns them into the row set. One
// statement is planned and executed once for any number of readings, where
// the per-row form below is planned once but executed, with its trigger,
// as a separate statement per reading. The performance baseline measured
// the per-row form at 0.33 ms a reading (docs/PERFORMANCE_OPTIMIZATIONS.md).
const insertMeasurementsMany = `
	INSERT INTO measurements (patient_id, type, value, unit, recorded_at, source, metadata)
	SELECT * FROM unnest($1::uuid[], $2::text[], $3::float8[], $4::text[], $5::timestamptz[], $6::text[], $7::jsonb[])
	RETURNING ` + measurementColumns

// CreateBatch stores several readings in one statement. Through a pool the
// insert is atomic; through a transaction it is part of it. The readings
// come back in input order. A rejected reading fails the whole statement
// with the rule it broke but without its index; CreateBatchEach names the
// index, at a cost per reading, and the store falls back to it on failure.
func (r *Measurements) CreateBatch(ctx context.Context, items []NewMeasurement) ([]domain.Measurement, error) {
	if len(items) == 0 {
		return []domain.Measurement{}, nil
	}
	n := len(items)
	patients := make([]string, n)
	types := make([]string, n)
	values := make([]float64, n)
	units := make([]string, n)
	recorded := make([]time.Time, n)
	sources := make([]string, n)
	metadata := make([]string, n)
	for i, m := range items {
		patients[i] = m.PatientID.String()
		types[i] = string(m.Type)
		values[i] = m.Value
		units[i] = m.Unit
		recorded[i] = m.RecordedAt
		sources[i] = m.Source
		if len(m.Metadata) == 0 {
			metadata[i] = "{}"
		} else {
			metadata[i] = string(m.Metadata)
		}
	}
	rows, err := r.db.Query(ctx, insertMeasurementsMany, patients, types, values, units, recorded, sources, metadata)
	stored, err := collectAll[domain.Measurement](rows, err)
	if err != nil {
		return nil, err
	}
	// RETURNING follows insertion order in practice but not by contract;
	// the reading's own key, unique within a batch, puts each row where its
	// input was.
	type key struct {
		patient uuid.UUID
		typ     domain.MeasurementType
		at      time.Time
		source  string
	}
	byKey := make(map[key]int, n)
	for i, m := range items {
		byKey[key{m.PatientID, m.Type, m.RecordedAt.UTC().Truncate(time.Microsecond), m.Source}] = i
	}
	ordered := make([]domain.Measurement, n)
	placed := 0
	for _, m := range stored {
		i, ok := byKey[key{m.PatientID, m.Type, m.RecordedAt.UTC().Truncate(time.Microsecond), m.Source}]
		if !ok {
			return nil, fmt.Errorf("stored reading %s does not match any input", m.ID)
		}
		ordered[i] = m
		placed++
	}
	if placed != n || len(stored) != n {
		return nil, fmt.Errorf("stored %d readings for %d inputs", len(stored), n)
	}
	return ordered, nil
}

// CreateBatchEach stores several readings as one statement per reading in
// one round trip. Through a pool the batch is atomic; through a transaction
// it is part of it. A rejected item is reported as a *BatchItemError naming
// its index, which is what this form is for.
func (r *Measurements) CreateBatchEach(ctx context.Context, items []NewMeasurement) ([]domain.Measurement, error) {
	if len(items) == 0 {
		return []domain.Measurement{}, nil
	}
	batch := &pgx.Batch{}
	for _, m := range items {
		batch.Queue(insertMeasurement, m.args()...)
	}
	return collectBatch[domain.Measurement](r.db.SendBatch(ctx, batch), len(items))
}

// ListForJob returns up to limit readings of one type in a time range,
// ordered by recording time only. It is the read behind processing: the
// (patient_id, type, recorded_at) prefix of the uniqueness index yields
// this order directly, where the listing's order by (recorded_at, id) made
// the planner sort the whole range, on disk past a few thousand rows. The
// processor orders by time and identifier itself before it computes
// anything, so the tiebreak is not needed here.
//
// Only what the processor is sent is read (id, type, value, unit and the
// recording time), scanned by position: the full row with its jsonb
// metadata through the reflective scan cost 200 to 300 ms and fifteen
// allocations a row for 60,000 rows, three times the query itself.
func (r *Measurements) ListForJob(ctx context.Context, patientID uuid.UUID, typ domain.MeasurementType, from, to *time.Time, limit int) ([]processing.Reading, error) {
	var sb strings.Builder
	sb.WriteString(`SELECT id, type, value, unit, recorded_at FROM measurements WHERE patient_id = $1 AND type = $2`)
	args := []any{patientID, typ}
	if from != nil {
		args = append(args, *from)
		sb.WriteString(` AND recorded_at >= $` + strconv.Itoa(len(args)))
	}
	if to != nil {
		args = append(args, *to)
		sb.WriteString(` AND recorded_at < $` + strconv.Itoa(len(args)))
	}
	args = append(args, limit)
	sb.WriteString(` ORDER BY recorded_at LIMIT $` + strconv.Itoa(len(args)))
	rows, err := r.db.Query(ctx, sb.String(), args...)
	if err != nil {
		return nil, mapError(err)
	}
	defer rows.Close()
	out := make([]processing.Reading, 0, min(limit, 4096))
	var (
		id uuid.UUID
		at time.Time
	)
	for rows.Next() {
		var m processing.Reading
		if err := rows.Scan(&id, &m.Type, &m.Value, &m.Unit, &at); err != nil {
			return nil, mapError(err)
		}
		m.ID = id.String()
		m.RecordedAt = processing.FormatRecordedAt(at)
		out = append(out, m)
	}
	if err := rows.Err(); err != nil {
		return nil, mapError(err)
	}
	return out, nil
}

// GetByID returns the reading or a not-found error.
func (r *Measurements) GetByID(ctx context.Context, id uuid.UUID) (domain.Measurement, error) {
	rows, err := r.db.Query(ctx, `SELECT `+measurementColumns+` FROM measurements WHERE id = $1`, id)
	return collectOne[domain.Measurement](rows, err)
}

// ListByPatient returns up to limit readings of one patient matching filter,
// in recording order, starting after the cursor when one is given. Every
// predicate is a fixed fragment; values only ever travel as parameters.
func (r *Measurements) ListByPatient(ctx context.Context, patientID uuid.UUID, filter MeasurementFilter, after *MeasurementCursor, limit int) ([]domain.Measurement, error) {
	var sb strings.Builder
	sb.WriteString(`SELECT ` + measurementColumns + ` FROM measurements WHERE patient_id = $1`)
	args := []any{patientID}
	next := func(v any) string {
		args = append(args, v)
		return "$" + strconv.Itoa(len(args))
	}

	if filter.Type != nil {
		sb.WriteString(` AND type = ` + next(*filter.Type))
	}
	if filter.From != nil {
		sb.WriteString(` AND recorded_at >= ` + next(*filter.From))
	}
	if filter.To != nil {
		sb.WriteString(` AND recorded_at < ` + next(*filter.To))
	}
	if after != nil {
		sb.WriteString(` AND (recorded_at, id) > (` + next(after.RecordedAt) + `, ` + next(after.ID) + `)`)
	}
	sb.WriteString(` ORDER BY recorded_at, id LIMIT ` + next(limit))

	rows, err := r.db.Query(ctx, sb.String(), args...)
	return collectAll[domain.Measurement](rows, err)
}

// Delete removes a reading, or returns a not-found error.
func (r *Measurements) Delete(ctx context.Context, id uuid.UUID) error {
	tag, err := r.db.Exec(ctx, `DELETE FROM measurements WHERE id = $1`, id)
	if err != nil {
		return mapError(err)
	}
	if tag.RowsAffected() == 0 {
		return domain.New(domain.KindNotFound, "MEASUREMENT_NOT_FOUND", "The measurement does not exist.")
	}
	return nil
}

// asDomain reports whether err is or wraps a *domain.Error, storing it in target.
func asDomain(err error, target **domain.Error) bool {
	return errors.As(err, target)
}
