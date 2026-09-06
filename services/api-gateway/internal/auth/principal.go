package auth

import (
	"context"
	"time"

	"github.com/google/uuid"

	"github.com/n0ah-n0wa/VitalMesh/services/api-gateway/internal/domain"
)

// Principal is the identity a request acts as, established from a verified
// access token. Authorization decisions are made from it, never from
// request input.
type Principal struct {
	UserID uuid.UUID
	Role   domain.Role
	// TokenID identifies the token the principal was established from.
	TokenID   string
	ExpiresAt time.Time
}

// PrincipalOf converts verified claims into a Principal.
func PrincipalOf(c Claims) Principal {
	return Principal{UserID: c.Subject, Role: c.Role, TokenID: c.TokenID, ExpiresAt: c.ExpiresAt}
}

type ctxKey struct{}

// NewContext returns a copy of ctx carrying p.
func NewContext(ctx context.Context, p Principal) context.Context {
	return context.WithValue(ctx, ctxKey{}, p)
}

// FromContext returns the principal carried by ctx, if any.
func FromContext(ctx context.Context) (Principal, bool) {
	p, ok := ctx.Value(ctxKey{}).(Principal)
	return p, ok
}
