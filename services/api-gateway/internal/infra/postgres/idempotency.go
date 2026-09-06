package postgres

import (
	"context"
	"encoding/json"
	"time"

	"github.com/google/uuid"

	"github.com/n0ah-n0wa/VitalMesh/services/api-gateway/internal/domain"
)

const idempotencyColumns = `id, user_id, method, path, key, request_fingerprint, status,
	response_status, response_body, created_at, expires_at`

// Idempotency persists idempotency records.
type Idempotency struct {
	db DB
}

// NewIdempotency returns a repository bound to db.
func NewIdempotency(db DB) *Idempotency { return &Idempotency{db: db} }

// NewIdempotencyKey is the input to Begin.
type NewIdempotencyKey struct {
	UserID             uuid.UUID
	Method             string
	Path               string
	Key                string
	RequestFingerprint string
	ExpiresAt          time.Time
}

// Begin records a new IN_PROGRESS key and reports created=true. If the key
// already exists it returns the existing record with created=false; the
// caller compares the fingerprint and either replays or reports a conflict.
// The unique constraint serialises concurrent replays.
func (r *Idempotency) Begin(ctx context.Context, k NewIdempotencyKey) (record domain.IdempotencyRecord, created bool, err error) {
	rows, err := r.db.Query(ctx, `
		INSERT INTO idempotency_keys (user_id, method, path, key, request_fingerprint, expires_at)
		VALUES ($1, $2, $3, $4, $5, $6)
		ON CONFLICT (user_id, method, path, key) DO NOTHING
		RETURNING `+idempotencyColumns,
		k.UserID, k.Method, k.Path, k.Key, k.RequestFingerprint, k.ExpiresAt)
	record, err = collectOne[domain.IdempotencyRecord](rows, err)
	if err == nil {
		return record, true, nil
	}
	var domErr *domain.Error
	if !asDomain(err, &domErr) || domErr.Kind != domain.KindNotFound {
		return domain.IdempotencyRecord{}, false, err
	}
	existing, err := r.Get(ctx, k.UserID, k.Method, k.Path, k.Key)
	return existing, false, err
}

// Get returns the record for a key or a not-found error.
func (r *Idempotency) Get(ctx context.Context, userID uuid.UUID, method, path, key string) (domain.IdempotencyRecord, error) {
	rows, err := r.db.Query(ctx, `
		SELECT `+idempotencyColumns+` FROM idempotency_keys
		WHERE user_id = $1 AND method = $2 AND path = $3 AND key = $4`,
		userID, method, path, key)
	return collectOne[domain.IdempotencyRecord](rows, err)
}

// Complete stores the response for an IN_PROGRESS record.
func (r *Idempotency) Complete(ctx context.Context, id uuid.UUID, responseStatus int, responseBody json.RawMessage) (domain.IdempotencyRecord, error) {
	rows, err := r.db.Query(ctx, `
		UPDATE idempotency_keys
		SET status = 'COMPLETED', response_status = $2, response_body = $3
		WHERE id = $1 AND status = 'IN_PROGRESS'
		RETURNING `+idempotencyColumns,
		id, responseStatus, responseBody)
	return collectOne[domain.IdempotencyRecord](rows, err)
}

// DeleteExpired removes up to limit of the oldest expired records and
// reports how many were deleted. The retention job calls it repeatedly
// until it returns 0, so each transaction stays short and the lock
// footprint bounded.
func (r *Idempotency) DeleteExpired(ctx context.Context, now time.Time, limit int) (int64, error) {
	// id = ANY(ARRAY(...)) makes the delete probe the primary key for the
	// selected ids; the IN (subquery) form plans as a hash semi-join that
	// scans the whole table.
	tag, err := r.db.Exec(ctx, `
		DELETE FROM idempotency_keys
		WHERE id = ANY(ARRAY(
			SELECT id FROM idempotency_keys
			WHERE expires_at <= $1
			ORDER BY expires_at
			LIMIT $2
		))`, now, limit)
	if err != nil {
		return 0, mapError(err)
	}
	return tag.RowsAffected(), nil
}

// Delete removes a record, for example one whose request failed before a
// response could be stored, so that the key can be used again.
func (r *Idempotency) Delete(ctx context.Context, id uuid.UUID) error {
	if _, err := r.db.Exec(ctx, `DELETE FROM idempotency_keys WHERE id = $1`, id); err != nil {
		return mapError(err)
	}
	return nil
}
