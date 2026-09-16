package postgres

import (
	"errors"
	"fmt"
	"io"
	"net"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/n0ah-n0wa/VitalMesh/services/api-gateway/internal/domain"
)

func TestMapErrorClassifiesConnectivityAsUnavailable(t *testing.T) {
	cases := []struct {
		name string
		err  error
	}{
		{"connect error", &pgconn.ConnectError{}},
		{"wrapped connect error", fmt.Errorf("begin transaction: %w", &pgconn.ConnectError{})},
		{"network error", &net.OpError{Op: "dial", Err: errors.New("connection refused")}},
		{"unexpected EOF", fmt.Errorf("query: %w", io.ErrUnexpectedEOF)},
		{"admin shutdown", &pgconn.PgError{Code: "57P01", Message: "terminating connection due to administrator command"}},
		{"cannot connect now", &pgconn.PgError{Code: "57P03"}},
		{"too many connections", &pgconn.PgError{Code: "53300"}},
		{"connection exception", &pgconn.PgError{Code: "08006"}},
		{"conn closed in words", errors.New("write failed: conn closed")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := mapError(tc.err)
			var domErr *domain.Error
			if !errors.As(got, &domErr) || domErr.Kind != domain.KindUnavailable || domErr.Code != "DATABASE_UNAVAILABLE" {
				t.Fatalf("mapError(%v) = %v, want KindUnavailable DATABASE_UNAVAILABLE", tc.err, got)
			}
			for _, secret := range []string{"5432", "postgres", "conn closed"} {
				if domErr.Message != "The database is unavailable; retry later." || containsFold(domErr.Message, secret) {
					t.Errorf("message %q leaks detail", domErr.Message)
				}
			}
			if !errors.Is(got, tc.err) {
				t.Error("the cause is not wrapped")
			}
		})
	}
}

func TestMapErrorLeavesTheRestAlone(t *testing.T) {
	if got := mapError(nil); got != nil {
		t.Errorf("nil -> %v", got)
	}
	var domErr *domain.Error
	if got := mapError(pgx.ErrNoRows); !errors.As(got, &domErr) || domErr.Kind != domain.KindNotFound {
		t.Errorf("ErrNoRows -> %v", got)
	}
	if got := mapError(&pgconn.PgError{Code: "23505", ConstraintName: "users_email_key"}); !errors.As(got, &domErr) || domErr.Kind != domain.KindConflict || domErr.Code != "USERS_EMAIL_KEY" {
		t.Errorf("unique violation -> %v", got)
	}
	plain := errors.New("something else entirely")
	if got := mapError(plain); got != plain {
		t.Errorf("an unclassified error must pass through, got %v", got)
	}
	if got := mapError(&pgconn.PgError{Code: "42P01"}); errors.As(got, &domErr) {
		t.Errorf("an undefined table is a bug, not unavailability: %v", got)
	}
}

func containsFold(s, sub string) bool {
	return len(sub) > 0 && len(s) >= len(sub) && (indexFold(s, sub) >= 0)
}

func indexFold(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if equalFold(s[i:i+len(sub)], sub) {
			return i
		}
	}
	return -1
}

func equalFold(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := 0; i < len(a); i++ {
		x, y := a[i], b[i]
		if 'A' <= x && x <= 'Z' {
			x += 'a' - 'A'
		}
		if 'A' <= y && y <= 'Z' {
			y += 'a' - 'A'
		}
		if x != y {
			return false
		}
	}
	return true
}
