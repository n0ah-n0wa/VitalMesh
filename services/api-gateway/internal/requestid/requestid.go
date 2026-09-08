// Package requestid generates, validates and carries request identifiers.
// It is transport-agnostic so that logging and outbound clients can use it
// without depending on the HTTP layer.
package requestid

import (
	"context"
	"crypto/rand"
)

// Header is the HTTP header carrying the request ID in both directions.
const Header = "X-Request-ID"

// CorrelationHeader carries the identifier that follows one client request
// across every service it touches (SPECIFICATIONS.md section 85). The
// request ID identifies a single hop; the correlation ID identifies the
// whole journey, so a failure in the processor can be tied to the call that
// caused it.
const CorrelationHeader = "X-Correlation-ID"

const maxLen = 128

type ctxKey struct{}

type correlationKey struct{}

// NewContext returns a copy of ctx carrying id.
func NewContext(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, ctxKey{}, id)
}

// FromContext returns the request ID carried by ctx, or "" if there is none.
func FromContext(ctx context.Context) string {
	id, _ := ctx.Value(ctxKey{}).(string)
	return id
}

// NewCorrelationContext returns a copy of ctx carrying the correlation ID.
func NewCorrelationContext(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, correlationKey{}, id)
}

// CorrelationFromContext returns the correlation ID carried by ctx, or ""
// if there is none.
func CorrelationFromContext(ctx context.Context) string {
	id, _ := ctx.Value(correlationKey{}).(string)
	return id
}

// Generate returns a new random identifier.
func Generate() string {
	return rand.Text()
}

// Valid reports whether id is acceptable as a client-supplied request ID:
// 1 to 128 characters from [A-Za-z0-9._-].
func Valid(id string) bool {
	if id == "" || len(id) > maxLen {
		return false
	}
	for i := 0; i < len(id); i++ {
		c := id[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '.', c == '_', c == '-':
		default:
			return false
		}
	}
	return true
}
