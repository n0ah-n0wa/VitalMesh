package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/n0ah-n0wa/VitalMesh/services/api-gateway/internal/domain"
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

// CreateBatch inserts readings in one round trip and returns them in input
// order. Either every reading is stored or none is: through a pool the batch
// runs in an implicit transaction, and through a transaction it is part of
// it. The first rejected reading's error is returned.
func (r *Measurements) CreateBatch(ctx context.Context, items []NewMeasurement) ([]domain.Measurement, error) {
	if len(items) == 0 {
		return []domain.Measurement{}, nil
	}
	batch := &pgx.Batch{}
	for _, m := range items {
		batch.Queue(insertMeasurement, m.args()...)
	}
	return collectBatch[domain.Measurement](r.db.SendBatch(ctx, batch), len(items))
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
