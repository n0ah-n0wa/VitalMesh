package domain

import (
	"errors"
	"fmt"
	"testing"
)

func TestErrorString(t *testing.T) {
	plain := New(KindNotFound, "PATIENT_NOT_FOUND", "The patient does not exist.")
	if got, want := plain.Error(), "PATIENT_NOT_FOUND: The patient does not exist."; got != want {
		t.Errorf("Error() = %q, want %q", got, want)
	}

	cause := errors.New("connection refused")
	wrapped := Wrap(cause, KindUnavailable, "DATABASE_UNAVAILABLE", "Try again later.")
	if got, want := wrapped.Error(), "DATABASE_UNAVAILABLE: connection refused"; got != want {
		t.Errorf("Error() = %q, want %q", got, want)
	}
}

func TestErrorUnwrapsThroughFmt(t *testing.T) {
	cause := errors.New("boom")
	err := fmt.Errorf("saving patient: %w", Wrap(cause, KindInternal, "SAVE_FAILED", "Could not save."))

	var domErr *Error
	if !errors.As(err, &domErr) {
		t.Fatal("errors.As did not find *Error")
	}
	if domErr.Code != "SAVE_FAILED" {
		t.Errorf("Code = %q, want SAVE_FAILED", domErr.Code)
	}
	if !errors.Is(err, cause) {
		t.Error("errors.Is did not reach the cause")
	}
}

func TestKindString(t *testing.T) {
	if got := KindValidation.String(); got != "validation" {
		t.Errorf("KindValidation.String() = %q", got)
	}
	if got := Kind(200).String(); got != "kind(200)" {
		t.Errorf("unknown kind String() = %q", got)
	}
}
