// Package patienttest provides an in-memory patient.Store for tests of the
// service and the transport, mirroring the PostgreSQL store's contract:
// unique external references, soft deletes, keyset order, and the audit
// event stored together with each state change.
package patienttest

import (
	"context"
	"sort"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/n0ah-n0wa/VitalMesh/services/api-gateway/internal/domain"
	"github.com/n0ah-n0wa/VitalMesh/services/api-gateway/internal/patient"
)

// Recorded is one audit event with the patient it concerned.
type Recorded struct {
	PatientID uuid.UUID
	Event     patient.AuditEvent
}

// MemoryStore is a patient.Store held in memory. Set Err to make every
// call fail with it, or FailAudit to make the audit step fail so that the
// atomicity contract can be exercised.
type MemoryStore struct {
	mu        sync.Mutex
	patients  map[uuid.UUID]domain.Patient
	Audit     []Recorded
	Err       error
	FailAudit error
	clock     time.Time
}

// NewMemoryStore returns an empty store.
func NewMemoryStore() *MemoryStore {
	return &MemoryStore{patients: map[uuid.UUID]domain.Patient{}, clock: time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)}
}

// Seed stores n patients created in order with references "ref-0"... and
// returns them in creation order.
func (m *MemoryStore) Seed(n int) []domain.Patient {
	out := make([]domain.Patient, 0, n)
	for i := range n {
		p, _ := m.Create(context.Background(), patient.NewPatient{
			ExternalReference: "ref-" + string(rune('a'+i)),
			DateOfBirth:       time.Date(1980, 1, 1, 0, 0, 0, 0, time.UTC),
			Sex:               domain.SexOther,
		}, patient.AuditEvent{ActorID: uuid.New(), Action: domain.AuditPatientCreated, RequestID: "seed"})
		out = append(out, p)
	}
	m.mu.Lock()
	m.Audit = nil
	m.mu.Unlock()
	return out
}

func (m *MemoryStore) tick() time.Time {
	m.clock = m.clock.Add(time.Second)
	return m.clock
}

// Create implements patient.Store.
func (m *MemoryStore) Create(_ context.Context, in patient.NewPatient, event patient.AuditEvent) (domain.Patient, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.Err != nil {
		return domain.Patient{}, m.Err
	}
	for _, p := range m.patients {
		if p.ExternalReference == in.ExternalReference {
			return domain.Patient{}, domain.New(domain.KindConflict, "PATIENTS_EXTERNAL_REFERENCE_KEY", "The resource already exists.")
		}
	}
	if m.FailAudit != nil {
		return domain.Patient{}, m.FailAudit
	}
	now := m.tick()
	p := domain.Patient{
		ID: uuid.New(), ExternalReference: in.ExternalReference, DateOfBirth: in.DateOfBirth, Sex: in.Sex,
		Status: domain.PatientActive, CreatedAt: now, UpdatedAt: now,
	}
	m.patients[p.ID] = p
	m.Audit = append(m.Audit, Recorded{PatientID: p.ID, Event: event})
	return p, nil
}

// GetByID implements patient.Store.
func (m *MemoryStore) GetByID(_ context.Context, id uuid.UUID) (domain.Patient, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.Err != nil {
		return domain.Patient{}, m.Err
	}
	p, ok := m.patients[id]
	if !ok {
		return domain.Patient{}, domain.New(domain.KindNotFound, "NOT_FOUND", "The requested resource does not exist.")
	}
	return p, nil
}

// List implements patient.Store.
func (m *MemoryStore) List(_ context.Context, after *patient.Cursor, limit int) ([]domain.Patient, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.Err != nil {
		return nil, m.Err
	}
	var all []domain.Patient
	for _, p := range m.patients {
		if p.Status == domain.PatientDeleted {
			continue
		}
		if after != nil && !(p.CreatedAt.After(after.CreatedAt) || (p.CreatedAt.Equal(after.CreatedAt) && p.ID.String() > after.ID.String())) {
			continue
		}
		all = append(all, p)
	}
	sort.Slice(all, func(i, j int) bool {
		if !all[i].CreatedAt.Equal(all[j].CreatedAt) {
			return all[i].CreatedAt.Before(all[j].CreatedAt)
		}
		return all[i].ID.String() < all[j].ID.String()
	})
	if len(all) > limit {
		all = all[:limit]
	}
	return all, nil
}

// SoftDelete implements patient.Store.
func (m *MemoryStore) SoftDelete(_ context.Context, id uuid.UUID, now time.Time, event patient.AuditEvent) (domain.Patient, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.Err != nil {
		return domain.Patient{}, m.Err
	}
	p, ok := m.patients[id]
	if !ok {
		return domain.Patient{}, domain.New(domain.KindNotFound, "NOT_FOUND", "The requested resource does not exist.")
	}
	if p.Status == domain.PatientDeleted {
		return domain.Patient{}, domain.New(domain.KindConflict, patient.CodeAlreadyDeleted, "The patient is already deleted.")
	}
	if m.FailAudit != nil {
		return domain.Patient{}, m.FailAudit
	}
	p.Status = domain.PatientDeleted
	p.DeletedAt = &now
	p.UpdatedAt = m.tick()
	m.patients[id] = p
	m.Audit = append(m.Audit, Recorded{PatientID: id, Event: event})
	return p, nil
}

// Get returns a stored patient for assertions.
func (m *MemoryStore) Get(id uuid.UUID) (domain.Patient, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	p, ok := m.patients[id]
	return p, ok
}
