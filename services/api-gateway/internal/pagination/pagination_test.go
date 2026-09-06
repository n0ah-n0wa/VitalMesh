package pagination

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/n0ah-n0wa/VitalMesh/services/api-gateway/internal/domain"
)

type keyset struct {
	CreatedAt time.Time `json:"created_at"`
	ID        string    `json:"id"`
}

func TestCursorRoundTrip(t *testing.T) {
	in := keyset{CreatedAt: time.Date(2026, 9, 6, 12, 0, 0, 0, time.UTC), ID: "abc"}

	cursor, err := EncodeCursor(in)
	if err != nil {
		t.Fatalf("EncodeCursor: %v", err)
	}
	if strings.ContainsAny(cursor, "+/=") {
		t.Errorf("cursor %q is not URL-safe", cursor)
	}

	var out keyset
	if err := DecodeCursor(cursor, &out); err != nil {
		t.Fatalf("DecodeCursor: %v", err)
	}
	if !out.CreatedAt.Equal(in.CreatedAt) || out.ID != in.ID {
		t.Errorf("round trip = %+v, want %+v", out, in)
	}
}

func TestDecodeCursorRejectsGarbage(t *testing.T) {
	for name, cursor := range map[string]string{
		"not base64":   "!!!",
		"not json":     "bm90IGpzb24", // "not json"
		"wrong shape":  "WzEsMl0",     // [1,2]
		"empty string": "",
	} {
		t.Run(name, func(t *testing.T) {
			var out keyset
			err := DecodeCursor(cursor, &out)
			var domErr *domain.Error
			if !errors.As(err, &domErr) || domErr.Kind != domain.KindValidation || domErr.Code != "INVALID_CURSOR" {
				t.Fatalf("DecodeCursor(%q) = %v, want INVALID_CURSOR validation error", cursor, err)
			}
			if len(domErr.Details) != 1 || domErr.Details[0].Field != "cursor" {
				t.Errorf("Details = %+v, want one entry for cursor", domErr.Details)
			}
		})
	}
}

func TestStandardLimits(t *testing.T) {
	l := StandardLimits()
	if l.Default <= 0 || l.Max < l.Default {
		t.Errorf("StandardLimits() = %+v is not sane", l)
	}
}
