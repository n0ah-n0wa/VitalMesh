package postgres

import (
	"context"
	"time"

	"github.com/google/uuid"

	"github.com/n0ah-n0wa/VitalMesh/services/api-gateway/internal/domain"
)

const patientColumns = `id, external_reference, date_of_birth, sex, status, created_at, updated_at, deleted_at`

// Patients persists synthetic patients.
type Patients struct {
	db DB
}

// NewPatients returns a repository bound to db.
func NewPatients(db DB) *Patients { return &Patients{db: db} }

// PatientCursor is the keyset position after which List continues.
type PatientCursor struct {
	CreatedAt time.Time `json:"created_at"`
	ID        uuid.UUID `json:"id"`
}

// Create inserts an active patient.
func (r *Patients) Create(ctx context.Context, externalReference string, dateOfBirth time.Time, sex domain.Sex) (domain.Patient, error) {
	rows, err := r.db.Query(ctx, `
		INSERT INTO patients (external_reference, date_of_birth, sex)
		VALUES ($1, $2, $3)
		RETURNING `+patientColumns,
		externalReference, dateOfBirth, sex)
	return collectOne[domain.Patient](rows, err)
}

// GetByID returns the patient regardless of status, or a not-found error.
func (r *Patients) GetByID(ctx context.Context, id uuid.UUID) (domain.Patient, error) {
	rows, err := r.db.Query(ctx, `SELECT `+patientColumns+` FROM patients WHERE id = $1`, id)
	return collectOne[domain.Patient](rows, err)
}

// List returns up to limit non-deleted patients ordered by creation, starting
// after the cursor when one is given. The order is stable under concurrent
// inserts because (created_at, id) is a total order.
func (r *Patients) List(ctx context.Context, after *PatientCursor, limit int) ([]domain.Patient, error) {
	if after == nil {
		rows, err := r.db.Query(ctx, `
			SELECT `+patientColumns+` FROM patients
			WHERE status <> 'DELETED'
			ORDER BY created_at, id
			LIMIT $1`, limit)
		return collectAll[domain.Patient](rows, err)
	}
	rows, err := r.db.Query(ctx, `
		SELECT `+patientColumns+` FROM patients
		WHERE status <> 'DELETED' AND (created_at, id) > ($1, $2)
		ORDER BY created_at, id
		LIMIT $3`, after.CreatedAt, after.ID, limit)
	return collectAll[domain.Patient](rows, err)
}

// SoftDelete marks the patient deleted at `now`. Deleting twice is a conflict.
func (r *Patients) SoftDelete(ctx context.Context, id uuid.UUID, now time.Time) (domain.Patient, error) {
	rows, err := r.db.Query(ctx, `
		UPDATE patients SET status = 'DELETED', deleted_at = $2
		WHERE id = $1 AND status <> 'DELETED'
		RETURNING `+patientColumns,
		id, now)
	patient, err := collectOne[domain.Patient](rows, err)
	if err != nil {
		return domain.Patient{}, r.explainMissingUpdate(ctx, id, err)
	}
	return patient, nil
}

// explainMissingUpdate turns a no-rows result of a conditional update into
// not-found or conflict, whichever applies.
func (r *Patients) explainMissingUpdate(ctx context.Context, id uuid.UUID, err error) error {
	var domErr *domain.Error
	if !asDomain(err, &domErr) || domErr.Kind != domain.KindNotFound {
		return err
	}
	if _, getErr := r.GetByID(ctx, id); getErr != nil {
		return getErr
	}
	return domain.New(domain.KindConflict, "PATIENT_ALREADY_DELETED", "The patient is already deleted.")
}
