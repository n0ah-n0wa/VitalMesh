// Package validate accumulates input validation failures and turns them into
// a single domain validation error that lists every offending field.
package validate

import (
	"cmp"
	"fmt"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/n0ah-n0wa/VitalMesh/services/api-gateway/internal/domain"
)

// Code and Message identify a validation failure in the error envelope.
const (
	Code    = "VALIDATION_FAILED"
	Message = "The request is invalid."
)

// Validator collects field errors. The zero value is ready to use.
type Validator struct {
	errs []domain.FieldError
}

// Add records a failure for field.
func (v *Validator) Add(field, message string) {
	v.errs = append(v.errs, domain.FieldError{Field: field, Message: message})
}

// Check records message for field unless ok.
func (v *Validator) Check(ok bool, field, message string) {
	if !ok {
		v.Add(field, message)
	}
}

// Required fails when value is empty or only whitespace.
func (v *Validator) Required(field, value string) {
	v.Check(strings.TrimSpace(value) != "", field, "is required")
}

// MaxLength fails when value has more than limit characters (runes).
func (v *Validator) MaxLength(field, value string, limit int) {
	v.Check(utf8.RuneCountInString(value) <= limit, field, fmt.Sprintf("must be at most %d characters", limit))
}

// OneOf fails when value is not one of allowed.
func (v *Validator) OneOf(field, value string, allowed ...string) {
	v.Check(slices.Contains(allowed, value), field, "must be one of: "+strings.Join(allowed, ", "))
}

// InRange fails when value is outside [lo, hi].
func InRange[T cmp.Ordered](v *Validator, field string, value, lo, hi T) {
	v.Check(value >= lo && value <= hi, field, fmt.Sprintf("must be between %v and %v", lo, hi))
}

// Valid reports whether no failure has been recorded.
func (v *Validator) Valid() bool { return len(v.errs) == 0 }

// Err returns nil when valid, otherwise a domain validation error carrying
// every recorded field error.
func (v *Validator) Err() error {
	if v.Valid() {
		return nil
	}
	return &domain.Error{
		Kind:    domain.KindValidation,
		Code:    Code,
		Message: Message,
		Details: slices.Clone(v.errs),
	}
}
