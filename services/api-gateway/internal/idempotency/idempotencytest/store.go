// Package idempotencytest provides an in-memory idempotency.Store.
package idempotencytest

import (
	"context"
	"encoding/json"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/n0ah-n0wa/VitalMesh/services/api-gateway/internal/domain"
	"github.com/n0ah-n0wa/VitalMesh/services/api-gateway/internal/idempotency"
)

type scope struct {
	user              uuid.UUID
	method, path, key string
}

// MemoryStore is an idempotency.Store held in memory. Set Err to make every
// call fail. Now supplies created_at; nil means time.Now.
//
// Like a real database driver, it refuses a call whose context has already
// ended. Callers that must finish their work after a client goes away have
// to detach from the request's context, and this double is what proves they
// do.
type MemoryStore struct {
	mu      sync.Mutex
	records map[scope]domain.IdempotencyRecord
	Err     error
	Now     func() time.Time
}

// NewMemoryStore returns an empty store.
func NewMemoryStore() *MemoryStore {
	return &MemoryStore{records: map[scope]domain.IdempotencyRecord{}}
}

func (m *MemoryStore) now() time.Time {
	if m.Now != nil {
		return m.Now()
	}
	return time.Now()
}

// Begin implements idempotency.Store.
func (m *MemoryStore) Begin(ctx context.Context, req idempotency.Request) (domain.IdempotencyRecord, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return domain.IdempotencyRecord{}, false, err
	}
	if m.Err != nil {
		return domain.IdempotencyRecord{}, false, m.Err
	}
	s := scope{req.UserID, req.Method, req.Path, req.Key}
	if existing, ok := m.records[s]; ok {
		return existing, false, nil
	}
	rec := domain.IdempotencyRecord{
		ID: uuid.New(), UserID: req.UserID, Method: req.Method, Path: req.Path, Key: req.Key,
		RequestFingerprint: req.Fingerprint, Status: domain.IdempotencyInProgress,
		CreatedAt: m.now(), ExpiresAt: req.ExpiresAt,
	}
	m.records[s] = rec
	return rec, true, nil
}

// Complete implements idempotency.Store.
func (m *MemoryStore) Complete(ctx context.Context, id uuid.UUID, status int, headers map[string]string, body json.RawMessage) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	if m.Err != nil {
		return m.Err
	}
	for s, rec := range m.records {
		if rec.ID == id {
			rec.Status = domain.IdempotencyCompleted
			rec.ResponseStatus = &status
			rec.ResponseHeaders = headers
			rec.ResponseBody = body
			m.records[s] = rec
			return nil
		}
	}
	return domain.New(domain.KindNotFound, "NOT_FOUND", "no such record")
}

// Delete implements idempotency.Store.
func (m *MemoryStore) Delete(ctx context.Context, id uuid.UUID) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	if m.Err != nil {
		return m.Err
	}
	for s, rec := range m.records {
		if rec.ID == id {
			delete(m.records, s)
		}
	}
	return nil
}

// Len returns the number of stored records.
func (m *MemoryStore) Len() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.records)
}

// Records returns a snapshot of the stored records.
func (m *MemoryStore) Records() []domain.IdempotencyRecord {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]domain.IdempotencyRecord, 0, len(m.records))
	for _, rec := range m.records {
		out = append(out, rec)
	}
	return out
}
