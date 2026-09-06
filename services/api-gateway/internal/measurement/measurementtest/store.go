// Package measurementtest provides an in-memory measurement.Store that
// mirrors the PostgreSQL store's contract: the (patient, type, recorded_at,
// source) uniqueness, all-or-nothing batches reporting the offending item,
// keyset order, and the audit event stored with each state change.
package measurementtest

import (
	"context"
	"sort"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/n0ah-n0wa/VitalMesh/services/api-gateway/internal/domain"
	"github.com/n0ah-n0wa/VitalMesh/services/api-gateway/internal/measurement"
)

// Recorded is one audit event with the reading it concerned.
type Recorded struct {
	MeasurementID uuid.UUID
	Event         measurement.AuditEvent
}

// MemoryStore is a measurement.Store held in memory. Patients holds the
// status of every known patient; unknown ids are not found. Set Err to make
// every call fail.
type MemoryStore struct {
	mu       sync.Mutex
	items    map[uuid.UUID]domain.Measurement
	Patients map[uuid.UUID]domain.PatientStatus
	Audit    []Recorded
	Err      error
	clock    time.Time
}

// NewMemoryStore returns an empty store.
func NewMemoryStore() *MemoryStore {
	return &MemoryStore{
		items:    map[uuid.UUID]domain.Measurement{},
		Patients: map[uuid.UUID]domain.PatientStatus{},
		clock:    time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC),
	}
}

// AddPatient registers a patient and returns its id.
func (m *MemoryStore) AddPatient(status domain.PatientStatus) uuid.UUID {
	m.mu.Lock()
	defer m.mu.Unlock()
	id := uuid.New()
	m.Patients[id] = status
	return id
}

type key struct {
	patient    uuid.UUID
	typ        domain.MeasurementType
	recordedAt time.Time
	source     string
}

func (m *MemoryStore) insert(in measurement.NewMeasurement) (domain.Measurement, error) {
	k := key{in.PatientID, in.Type, in.RecordedAt, in.Source}
	for _, existing := range m.items {
		if (key{existing.PatientID, existing.Type, existing.RecordedAt, existing.Source}) == k {
			return domain.Measurement{}, domain.New(domain.KindConflict, "MEASUREMENTS_PATIENT_TYPE_TIME_SOURCE_KEY", "The resource already exists.")
		}
	}
	m.clock = m.clock.Add(time.Millisecond)
	created := domain.Measurement{
		ID: uuid.New(), PatientID: in.PatientID, Type: in.Type, Value: in.Value, Unit: in.Unit,
		RecordedAt: in.RecordedAt, CreatedAt: m.clock, Source: in.Source, Metadata: in.Metadata,
	}
	m.items[created.ID] = created
	return created, nil
}

// Create implements measurement.Store.
func (m *MemoryStore) Create(_ context.Context, in measurement.NewMeasurement, event measurement.AuditEvent) (domain.Measurement, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.Err != nil {
		return domain.Measurement{}, m.Err
	}
	created, err := m.insert(in)
	if err != nil {
		return domain.Measurement{}, err
	}
	m.Audit = append(m.Audit, Recorded{created.ID, event})
	return created, nil
}

// CreateBatch implements measurement.Store.
func (m *MemoryStore) CreateBatch(_ context.Context, in []measurement.NewMeasurement, event measurement.AuditEvent) ([]domain.Measurement, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.Err != nil {
		return nil, m.Err
	}
	created := make([]domain.Measurement, 0, len(in))
	for i, item := range in {
		c, err := m.insert(item)
		if err != nil {
			for _, done := range created { // roll back
				delete(m.items, done.ID)
			}
			return nil, &measurement.ItemError{Index: i, Err: err}
		}
		created = append(created, c)
	}
	for _, c := range created {
		m.Audit = append(m.Audit, Recorded{c.ID, event})
	}
	return created, nil
}

// GetByID implements measurement.Store.
func (m *MemoryStore) GetByID(_ context.Context, id uuid.UUID) (domain.Measurement, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.Err != nil {
		return domain.Measurement{}, m.Err
	}
	item, ok := m.items[id]
	if !ok {
		return domain.Measurement{}, domain.New(domain.KindNotFound, "NOT_FOUND", "The requested resource does not exist.")
	}
	return item, nil
}

// ListByPatient implements measurement.Store.
func (m *MemoryStore) ListByPatient(_ context.Context, patientID uuid.UUID, filter measurement.Filter, after *measurement.Cursor, limit int) ([]domain.Measurement, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.Err != nil {
		return nil, m.Err
	}
	var out []domain.Measurement
	for _, item := range m.items {
		if item.PatientID != patientID {
			continue
		}
		if filter.Type != nil && item.Type != *filter.Type {
			continue
		}
		if filter.From != nil && item.RecordedAt.Before(*filter.From) {
			continue
		}
		if filter.To != nil && !item.RecordedAt.Before(*filter.To) {
			continue
		}
		if after != nil && !(item.RecordedAt.After(after.RecordedAt) || (item.RecordedAt.Equal(after.RecordedAt) && item.ID.String() > after.ID.String())) {
			continue
		}
		out = append(out, item)
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].RecordedAt.Equal(out[j].RecordedAt) {
			return out[i].RecordedAt.Before(out[j].RecordedAt)
		}
		return out[i].ID.String() < out[j].ID.String()
	})
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

// Delete implements measurement.Store.
func (m *MemoryStore) Delete(_ context.Context, id uuid.UUID, event measurement.AuditEvent) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.Err != nil {
		return m.Err
	}
	if _, ok := m.items[id]; !ok {
		return domain.New(domain.KindNotFound, "MEASUREMENT_NOT_FOUND", "The measurement does not exist.")
	}
	delete(m.items, id)
	m.Audit = append(m.Audit, Recorded{id, event})
	return nil
}

// PatientStatus implements measurement.Store.
func (m *MemoryStore) PatientStatus(_ context.Context, id uuid.UUID) (domain.PatientStatus, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.Err != nil {
		return "", m.Err
	}
	status, ok := m.Patients[id]
	if !ok {
		return "", domain.New(domain.KindNotFound, "NOT_FOUND", "The requested resource does not exist.")
	}
	return status, nil
}

// Len returns the number of stored readings.
func (m *MemoryStore) Len() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.items)
}
