package postgres

import (
	"context"
	"encoding/json"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

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
	batch := &pgx.Batch{}
	for _, e := range entries {
		metadata := e.Metadata
		if len(metadata) == 0 {
			metadata = json.RawMessage(`{}`)
		}
		batch.Queue(`
			INSERT INTO audit_logs (actor_id, actor_type, action, resource_type, resource_id, request_id, metadata)
			VALUES ($1, $2, $3, $4, $5, $6, $7)
			RETURNING `+auditColumns,
			e.ActorID, e.ActorType, e.Action, e.ResourceType, e.ResourceID, e.RequestID, metadata)
	}
	return collectBatch[domain.AuditEntry](r.db.SendBatch(ctx, batch), len(entries))
}
