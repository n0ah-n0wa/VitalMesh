// Package pagination holds the cursor pagination primitives shared by
// collection endpoints. Cursors are opaque to clients: repositories define
// their keyset payload and encode it with EncodeCursor. Cursors are not
// signed, so a decoded payload is client input: repositories must validate
// its fields exactly as they would validate query parameters.
package pagination

import (
	"encoding/base64"
	"encoding/json"

	"github.com/n0ah-n0wa/VitalMesh/services/api-gateway/internal/domain"
)

// Page size bounds and the longest cursor accepted from a client.
const (
	DefaultLimit    = 50
	MaxLimit        = 200
	MaxCursorLength = 512
)

// Limits bounds the page size an endpoint accepts.
type Limits struct {
	Default int
	Max     int
}

// StandardLimits returns the bounds most endpoints use.
func StandardLimits() Limits {
	return Limits{Default: DefaultLimit, Max: MaxLimit}
}

// Request is a parsed pagination request.
type Request struct {
	Limit  int
	Cursor string
}

// EncodeCursor turns a keyset payload into an opaque cursor string.
func EncodeCursor(payload any) (string, error) {
	data, err := json.Marshal(payload)
	if err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(data), nil
}

// DecodeCursor parses a cursor produced by EncodeCursor into payload. A
// cursor that cannot be decoded is reported as a validation error.
func DecodeCursor(cursor string, payload any) error {
	data, err := base64.RawURLEncoding.DecodeString(cursor)
	if err != nil {
		return invalidCursor()
	}
	if err := json.Unmarshal(data, payload); err != nil {
		return invalidCursor()
	}
	return nil
}

func invalidCursor() *domain.Error {
	return &domain.Error{
		Kind:    domain.KindValidation,
		Code:    "INVALID_CURSOR",
		Message: "The pagination cursor is not valid.",
		Details: []domain.FieldError{{Field: "cursor", Message: "is not a valid cursor"}},
	}
}
