// Package idempotency implements SPECIFICATIONS.md section 24 for write
// endpoints: a client may send an Idempotency-Key; the first request under
// a key runs and its response is stored; a repeat with the same body gets
// the stored response back; a repeat with a different body is refused. The
// record of truth is the idempotency_keys table (OQ-10); this package holds
// the rules and the persistence port, the HTTP middleware applies them.
package idempotency

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"time"

	"github.com/google/uuid"

	"github.com/n0ah-n0wa/VitalMesh/services/api-gateway/internal/domain"
)

// Header carries the client's key.
const Header = "Idempotency-Key"

// MaxKeyLength matches the schema.
const MaxKeyLength = 255

// Error codes.
const (
	CodeInvalidKey = "INVALID_IDEMPOTENCY_KEY"
	CodeKeyReused  = "IDEMPOTENCY_KEY_REUSED"
	CodeInProgress = "IDEMPOTENCY_IN_PROGRESS"
)

// ErrInvalidKey rejects a key outside the accepted shape.
func ErrInvalidKey() *domain.Error {
	e := domain.New(domain.KindValidation, CodeInvalidKey, "The Idempotency-Key header is invalid.")
	e.Details = []domain.FieldError{{Field: Header, Message: "must be 1 to 255 characters of [A-Za-z0-9._:-]"}}
	return e
}

// ErrKeyReused rejects a key replayed with a different request.
func ErrKeyReused() *domain.Error {
	return domain.New(domain.KindValidation, CodeKeyReused, "The Idempotency-Key was already used with a different request.")
}

// ErrInProgress rejects a replay while the first request is still running.
func ErrInProgress() *domain.Error {
	return domain.New(domain.KindConflict, CodeInProgress, "A request with this Idempotency-Key is still in progress; retry shortly.")
}

// ValidKey reports whether key has the accepted shape: 1 to MaxKeyLength
// characters of [A-Za-z0-9._:-].
func ValidKey(key string) bool {
	if key == "" || len(key) > MaxKeyLength {
		return false
	}
	for i := 0; i < len(key); i++ {
		c := key[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '.', c == '_', c == ':', c == '-':
		default:
			return false
		}
	}
	return true
}

// Fingerprint identifies a request's intent: method, request target and
// the exact body bytes. Two requests with equal fingerprints are the same
// request.
//
// The target is the path with its query string, not the path alone: two
// requests that differ only in a query parameter are different requests,
// and treating them as the same would replay one's response for the other.
func Fingerprint(method, target string, body []byte) string {
	h := sha256.New()
	h.Write([]byte(method))
	h.Write([]byte{0})
	h.Write([]byte(target))
	h.Write([]byte{0})
	h.Write(body)
	return hex.EncodeToString(h.Sum(nil))
}

// Request identifies one idempotent request.
type Request struct {
	UserID      uuid.UUID
	Method      string
	Path        string
	Key         string
	Fingerprint string
	ExpiresAt   time.Time
}

// Store is the persistence port, implemented over idempotency_keys.
type Store interface {
	// Begin records req unless a record for (user, method, path, key)
	// exists. It returns the record and whether it was created now.
	Begin(ctx context.Context, req Request) (record domain.IdempotencyRecord, created bool, err error)
	// Complete stores the response of a record that is in progress.
	// Headers are the allow-listed response headers to replay with it.
	Complete(ctx context.Context, id uuid.UUID, status int, headers map[string]string, body json.RawMessage) error
	// Delete removes a record so its key can be used again.
	Delete(ctx context.Context, id uuid.UUID) error
}
