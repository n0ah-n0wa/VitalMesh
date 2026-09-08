package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/n0ah-n0wa/VitalMesh/services/api-gateway/internal/domain"
	"github.com/n0ah-n0wa/VitalMesh/services/api-gateway/internal/idempotency"
	"github.com/n0ah-n0wa/VitalMesh/services/api-gateway/internal/measurement"
)

// MeasurementStore implements measurement.Store: reads go to the pool,
// every state change and its audit records share one transaction.
type MeasurementStore struct {
	pool *pgxpool.Pool
}

// NewMeasurementStore returns a store over pool.
func NewMeasurementStore(pool *pgxpool.Pool) *MeasurementStore {
	return &MeasurementStore{pool: pool}
}

// Create inserts one reading and its audit record atomically.
func (s *MeasurementStore) Create(ctx context.Context, in measurement.NewMeasurement, event measurement.AuditEvent) (domain.Measurement, error) {
	var created domain.Measurement
	err := WithTx(ctx, s.pool, func(tx pgx.Tx) error {
		var err error
		created, err = NewMeasurements(tx).Create(ctx, toNewMeasurement(in))
		if err != nil {
			return err
		}
		_, err = NewAudit(tx).Append(ctx, measurementAudit(created, event))
		return err
	})
	if err != nil {
		return domain.Measurement{}, err
	}
	return created, nil
}

// CreateBatch inserts every reading and one audit record each, or nothing.
// A rejected reading is reported as a *measurement.ItemError.
func (s *MeasurementStore) CreateBatch(ctx context.Context, in []measurement.NewMeasurement, event measurement.AuditEvent) ([]domain.Measurement, error) {
	items := make([]NewMeasurement, len(in))
	for i, m := range in {
		items[i] = toNewMeasurement(m)
	}
	var created []domain.Measurement
	err := WithTx(ctx, s.pool, func(tx pgx.Tx) error {
		var err error
		created, err = NewMeasurements(tx).CreateBatch(ctx, items)
		if err != nil {
			return err
		}
		entries := make([]NewAuditEntry, len(created))
		for i, m := range created {
			entries[i] = measurementAudit(m, event)
		}
		_, err = NewAudit(tx).AppendBatch(ctx, entries)
		return err
	})
	if err != nil {
		var item *BatchItemError
		if errors.As(err, &item) {
			return nil, &measurement.ItemError{Index: item.Index, Err: item.Err}
		}
		return nil, err
	}
	return created, nil
}

// GetByID returns one reading.
func (s *MeasurementStore) GetByID(ctx context.Context, id uuid.UUID) (domain.Measurement, error) {
	return NewMeasurements(s.pool).GetByID(ctx, id)
}

// ListByPatient returns a patient's readings after the cursor.
func (s *MeasurementStore) ListByPatient(ctx context.Context, patientID uuid.UUID, filter measurement.Filter, after *measurement.Cursor, limit int) ([]domain.Measurement, error) {
	var cursor *MeasurementCursor
	if after != nil {
		cursor = &MeasurementCursor{RecordedAt: after.RecordedAt, ID: after.ID}
	}
	return NewMeasurements(s.pool).ListByPatient(ctx, patientID, MeasurementFilter{Type: filter.Type, From: filter.From, To: filter.To}, cursor, limit)
}

// Delete removes one reading and records the audit event atomically.
func (s *MeasurementStore) Delete(ctx context.Context, id uuid.UUID, event measurement.AuditEvent) error {
	return WithTx(ctx, s.pool, func(tx pgx.Tx) error {
		existing, err := NewMeasurements(tx).GetByID(ctx, id)
		if err != nil {
			return err
		}
		if err := NewMeasurements(tx).Delete(ctx, id); err != nil {
			return err
		}
		_, err = NewAudit(tx).Append(ctx, measurementAudit(existing, event))
		return err
	})
}

// PatientStatus returns the patient's status.
func (s *MeasurementStore) PatientStatus(ctx context.Context, id uuid.UUID) (domain.PatientStatus, error) {
	p, err := NewPatients(s.pool).GetByID(ctx, id)
	if err != nil {
		return "", err
	}
	return p.Status, nil
}

func toNewMeasurement(m measurement.NewMeasurement) NewMeasurement {
	return NewMeasurement{
		PatientID: m.PatientID, Type: m.Type, Value: m.Value, Unit: m.Unit,
		RecordedAt: m.RecordedAt, Source: m.Source, Metadata: m.Metadata,
	}
}

func measurementAudit(m domain.Measurement, event measurement.AuditEvent) NewAuditEntry {
	actor := event.ActorID
	id := m.ID
	metadata, _ := json.Marshal(map[string]any{"patient_id": m.PatientID, "type": m.Type})
	return NewAuditEntry{
		ActorID:      &actor,
		ActorType:    domain.ActorUser,
		Action:       event.Action,
		ResourceType: measurement.ResourceType,
		ResourceID:   &id,
		RequestID:    event.RequestID,
		Metadata:     metadata,
	}
}

// IdempotencyStore adapts the idempotency_keys repository to the
// idempotency.Store port.
type IdempotencyStore struct {
	repo *Idempotency
}

// NewIdempotencyStore returns a store over pool.
func NewIdempotencyStore(pool *pgxpool.Pool) *IdempotencyStore {
	return &IdempotencyStore{repo: NewIdempotency(pool)}
}

// Begin implements idempotency.Store.
func (s *IdempotencyStore) Begin(ctx context.Context, req idempotency.Request) (domain.IdempotencyRecord, bool, error) {
	return s.repo.Begin(ctx, NewIdempotencyKey{
		UserID: req.UserID, Method: req.Method, Path: req.Path, Key: req.Key,
		RequestFingerprint: req.Fingerprint, ExpiresAt: req.ExpiresAt,
	})
}

// Complete implements idempotency.Store.
func (s *IdempotencyStore) Complete(ctx context.Context, id uuid.UUID, status int, headers map[string]string, body json.RawMessage) error {
	_, err := s.repo.Complete(ctx, id, status, headers, body)
	return err
}

// DeleteExpired implements idempotency.Expirer.
func (s *IdempotencyStore) DeleteExpired(ctx context.Context, now time.Time, limit int) (int64, error) {
	return s.repo.DeleteExpired(ctx, now, limit)
}

// Delete implements idempotency.Store.
func (s *IdempotencyStore) Delete(ctx context.Context, id uuid.UUID) error {
	return s.repo.Delete(ctx, id)
}
