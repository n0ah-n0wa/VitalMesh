// Package domain holds the vocabulary shared by every feature of the gateway.
// It depends on nothing else in the service. Today it defines the error model;
// entities and value objects join it as features are implemented.
package domain

import "fmt"

// Kind classifies an error so that transports can map it to a response
// without inspecting its cause.
type Kind uint8

// Error kinds, in the order transports usually check them.
const (
	// KindInternal is an unexpected failure. Its cause is logged but never
	// sent to clients.
	KindInternal Kind = iota
	// KindInvalid is a malformed request (bad syntax, unreadable body).
	KindInvalid
	// KindValidation is a well-formed request that violates a rule.
	KindValidation
	KindNotFound
	KindConflict
	KindUnauthorized
	KindForbidden
	KindRateLimited
	// KindTooLarge is a request or payload that exceeds a configured size.
	KindTooLarge
	// KindUnsupportedMedia is a request body in a format the API does not accept.
	KindUnsupportedMedia
	// KindUnavailable means a required dependency cannot serve the request.
	KindUnavailable
	KindTimeout
)

var kindNames = [...]string{
	KindInternal:         "internal",
	KindInvalid:          "invalid",
	KindValidation:       "validation",
	KindNotFound:         "not_found",
	KindConflict:         "conflict",
	KindUnauthorized:     "unauthorized",
	KindForbidden:        "forbidden",
	KindRateLimited:      "rate_limited",
	KindTooLarge:         "too_large",
	KindUnsupportedMedia: "unsupported_media",
	KindUnavailable:      "unavailable",
	KindTimeout:          "timeout",
}

func (k Kind) String() string {
	if int(k) < len(kindNames) {
		return kindNames[k]
	}
	return fmt.Sprintf("kind(%d)", k)
}

// FieldError points at one invalid input field. Both values are safe to
// return to clients.
type FieldError struct {
	Field   string
	Message string
}

// Error is a classified error carrying a stable, client-safe code and message
// plus an optional internal cause.
type Error struct {
	Kind Kind
	// Code is a stable machine-readable identifier such as PATIENT_NOT_FOUND.
	Code string
	// Message is human-readable and safe to return to clients.
	Message string
	// Details optionally names the input fields that caused the error. It is
	// safe to return to clients.
	Details []FieldError
	// Err is the underlying cause. It is never exposed to clients.
	Err error
}

// New returns an Error without an underlying cause.
func New(kind Kind, code, message string) *Error {
	return &Error{Kind: kind, Code: code, Message: message}
}

// Wrap returns an Error whose cause is err.
func Wrap(err error, kind Kind, code, message string) *Error {
	return &Error{Kind: kind, Code: code, Message: message, Err: err}
}

func (e *Error) Error() string {
	if e.Err != nil {
		return e.Code + ": " + e.Err.Error()
	}
	return e.Code + ": " + e.Message
}

// Unwrap exposes the cause to errors.Is and errors.As.
func (e *Error) Unwrap() error { return e.Err }
