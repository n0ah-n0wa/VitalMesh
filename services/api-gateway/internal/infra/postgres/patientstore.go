package postgres

import (
	"context"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/n0ah-n0wa/VitalMesh/services/api-gateway/internal/domain"
	"github.com/n0ah-n0wa/VitalMesh/services/api-gateway/internal/patient"
)

// PatientStore implements patient.Store: reads go straight to the pool,
// state changes and their audit record share one transaction
// (SPECIFICATIONS.md sections 22 and 43).
type PatientStore struct {
	pool *pgxpool.Pool
}

// NewPatientStore returns a store over pool.
func NewPatientStore(pool *pgxpool.Pool) *PatientStore {
	return &PatientStore{pool: pool}
}

// Create inserts the patient and appends the audit event atomically.
func (s *PatientStore) Create(ctx context.Context, in patient.NewPatient, event patient.AuditEvent) (domain.Patient, error) {
	var created domain.Patient
	err := WithTx(ctx, s.pool, func(tx pgx.Tx) error {
		var err error
		created, err = NewPatients(tx).Create(ctx, in.ExternalReference, in.DateOfBirth, in.Sex)
		if err != nil {
			return err
		}
		return appendPatientAudit(ctx, tx, created.ID, event)
	})
	if err != nil {
		return domain.Patient{}, err
	}
	return created, nil
}

// GetByID returns the patient whatever its status.
func (s *PatientStore) GetByID(ctx context.Context, id uuid.UUID) (domain.Patient, error) {
	return NewPatients(s.pool).GetByID(ctx, id)
}

// List returns non-deleted patients in creation order after the cursor.
func (s *PatientStore) List(ctx context.Context, after *patient.Cursor, limit int) ([]domain.Patient, error) {
	var cursor *PatientCursor
	if after != nil {
		cursor = &PatientCursor{CreatedAt: after.CreatedAt, ID: after.ID}
	}
	return NewPatients(s.pool).List(ctx, cursor, limit)
}

// SoftDelete marks the patient deleted and appends the audit event
// atomically.
func (s *PatientStore) SoftDelete(ctx context.Context, id uuid.UUID, now time.Time, event patient.AuditEvent) (domain.Patient, error) {
	var deleted domain.Patient
	err := WithTx(ctx, s.pool, func(tx pgx.Tx) error {
		var err error
		deleted, err = NewPatients(tx).SoftDelete(ctx, id, now)
		if err != nil {
			return err
		}
		return appendPatientAudit(ctx, tx, deleted.ID, event)
	})
	if err != nil {
		return domain.Patient{}, err
	}
	return deleted, nil
}

func appendPatientAudit(ctx context.Context, tx pgx.Tx, patientID uuid.UUID, event patient.AuditEvent) error {
	actor := event.ActorID
	_, err := NewAudit(tx).Append(ctx, NewAuditEntry{
		ActorID:      &actor,
		ActorType:    domain.ActorUser,
		Action:       event.Action,
		ResourceType: patient.ResourceType,
		ResourceID:   &patientID,
		RequestID:    event.RequestID,
	})
	return err
}
