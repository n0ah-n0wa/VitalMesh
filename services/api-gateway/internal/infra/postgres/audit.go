package postgres

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/google/uuid"

	"github.com/n0ah-n0wa/VitalMesh/services/api-gateway/internal/domain"
)

const auditColumns = `id, actor_id, actor_type, action, resource_type, resource_id, request_id, metadata, created_at`

// Audit persists the append-only audit log.
type Audit struct {
	db DB
}

// NewAudit returns a repository bound to db.
func NewAudit(db DB) *Audit { return &Audit{db: db} }

// NewAuditEntry is the input to Append. ActorID is required for ActorUser
// and must be nil for ActorSystem.
type NewAuditEntry struct {
	ActorID      *uuid.UUID
	ActorType    domain.ActorType
	Action       domain.AuditAction
	ResourceType string
	ResourceID   *uuid.UUID
	RequestID    string
	Metadata     json.RawMessage
}

// Append records an entry. Entries can never be changed afterwards.
func (r *Audit) Append(ctx context.Context, e NewAuditEntry) (domain.AuditEntry, error) {
	metadata := e.Metadata
	if len(metadata) == 0 {
		metadata = json.RawMessage(`{}`)
	}
	rows, err := r.db.Query(ctx, `
		INSERT INTO audit_logs (actor_id, actor_type, action, resource_type, resource_id, request_id, metadata)
		VALUES ($1, $2, $3, $4, $5, $6, $7)
		RETURNING `+auditColumns,
		e.ActorID, e.ActorType, e.Action, e.ResourceType, e.ResourceID, e.RequestID, metadata)
	return collectOne[domain.AuditEntry](rows, err)
}

// ListByResource returns the newest entries about a resource, newest first.
func (r *Audit) ListByResource(ctx context.Context, resourceID uuid.UUID, limit int) ([]domain.AuditEntry, error) {
	rows, err := r.db.Query(ctx, `
		SELECT `+auditColumns+` FROM audit_logs
		WHERE resource_id = $1
		ORDER BY created_at DESC, id DESC
		LIMIT $2`, resourceID, limit)
	return collectAll[domain.AuditEntry](rows, err)
}

// AppendBatch records several entries in one round trip. Through a pool the
// batch is atomic; through a transaction it is part of it.
func (r *Audit) AppendBatch(ctx context.Context, entries []NewAuditEntry) ([]domain.AuditEntry, error) {
	if len(entries) == 0 {
		return []domain.AuditEntry{}, nil
	}
	// One statement for the whole batch (see insertMeasurementsMany): the
	// entries travel as parallel arrays. Nullable identifiers travel as
	// pointers, so an absent actor or resource stays NULL.
	n := len(entries)
	actors := make([]*string, n)
	actorTypes := make([]string, n)
	actions := make([]string, n)
	resourceTypes := make([]string, n)
	resources := make([]*string, n)
	requests := make([]string, n)
	metadata := make([]string, n)
	for i, e := range entries {
		if e.ActorID != nil {
			s := e.ActorID.String()
			actors[i] = &s
		}
		actorTypes[i] = string(e.ActorType)
		actions[i] = string(e.Action)
		resourceTypes[i] = e.ResourceType
		if e.ResourceID != nil {
			s := e.ResourceID.String()
			resources[i] = &s
		}
		requests[i] = e.RequestID
		if len(e.Metadata) == 0 {
			metadata[i] = "{}"
		} else {
			metadata[i] = string(e.Metadata)
		}
	}
	rows, err := r.db.Query(ctx, `
		INSERT INTO audit_logs (actor_id, actor_type, action, resource_type, resource_id, request_id, metadata)
		SELECT * FROM unnest($1::uuid[], $2::text[], $3::text[], $4::text[], $5::uuid[], $6::text[], $7::jsonb[])
		RETURNING `+auditColumns,
		actors, actorTypes, actions, resourceTypes, resources, requests, metadata)
	stored, err := collectAll[domain.AuditEntry](rows, err)
	if err != nil {
		return nil, err
	}
	if len(stored) != n {
		return nil, fmt.Errorf("stored %d audit entries for %d inputs", len(stored), n)
	}
	return stored, nil
}
