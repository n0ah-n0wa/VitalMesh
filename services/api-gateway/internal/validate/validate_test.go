package validate

import (
	"errors"
	"testing"

	"github.com/n0ah-n0wa/VitalMesh/services/api-gateway/internal/domain"
)

func TestValidWhenNothingFails(t *testing.T) {
	var v Validator
	v.Required("name", "x")
	v.MaxLength("name", "héllo", 5)
	v.OneOf("sex", "F", "F", "M")
	InRange(&v, "age", 30, 0, 120)
	v.Check(true, "any", "never")

	if !v.Valid() || v.Err() != nil {
		t.Fatalf("expected valid, got %v", v.Err())
	}
}

func TestErrListsEveryFailure(t *testing.T) {
	var v Validator
	v.Required("name", "   ")
	v.MaxLength("note", "toolong", 3)
	v.OneOf("sex", "X", "F", "M")
	InRange(&v, "age", 200, 0, 120)
	InRange(&v, "ratio", 1.5, 0.0, 1.0)
	v.Check(false, "flag", "custom message")

	err := v.Err()
	var domErr *domain.Error
	if !errors.As(err, &domErr) {
		t.Fatalf("Err() = %T, want *domain.Error", err)
	}
	if domErr.Kind != domain.KindValidation || domErr.Code != Code || domErr.Message != Message {
		t.Errorf("unexpected classification: %+v", domErr)
	}

	want := []domain.FieldError{
		{Field: "name", Message: "is required"},
		{Field: "note", Message: "must be at most 3 characters"},
		{Field: "sex", Message: "must be one of: F, M"},
		{Field: "age", Message: "must be between 0 and 120"},
		{Field: "ratio", Message: "must be between 0 and 1"},
		{Field: "flag", Message: "custom message"},
	}
	if len(domErr.Details) != len(want) {
		t.Fatalf("Details = %+v, want %+v", domErr.Details, want)
	}
	for i := range want {
		if domErr.Details[i] != want[i] {
			t.Errorf("Details[%d] = %+v, want %+v", i, domErr.Details[i], want[i])
		}
	}
}

func TestErrDetailsAreDetached(t *testing.T) {
	var v Validator
	v.Add("a", "x")
	err := v.Err().(*domain.Error)
	v.Add("b", "y")
	if len(err.Details) != 1 {
		t.Errorf("Details grew after Err(): %+v", err.Details)
	}
}

func TestEmail(t *testing.T) {
	for _, ok := range []string{"a@b", "alice@example.com", "first.last+tag@sub.example.co.uk", "ünïcode@example.org"} {
		if !IsEmail(ok) {
			t.Errorf("IsEmail(%q) = false", ok)
		}
	}
	for _, bad := range []string{"alice", "@example.com", "alice@", "a@@b", "al ice@example.com", "alice@exam\tple.com", "alice@example.com\n"} {
		if IsEmail(bad) {
			t.Errorf("IsEmail(%q) = true", bad)
		}
	}

	var v Validator
	v.Email("email", "")
	v.Email("email", "fine@example.com")
	if !v.Valid() {
		t.Errorf("empty or valid email recorded a failure: %v", v.Err())
	}
	v.Email("email", "nope")
	if err := v.Err(); err == nil || err.(*domain.Error).Details[0].Message != "must be a valid email address" {
		t.Errorf("invalid email: %v", err)
	}
}
