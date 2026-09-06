// Package model defines the JSON bodies exchanged with clients. Nothing here
// carries behaviour; handlers translate between these types and the
// application layer.
package model

import (
	"encoding/json"
	"time"

	"github.com/google/uuid"
)

// ErrorResponse is the envelope returned for every failed request.
type ErrorResponse struct {
	Error ErrorDetail `json:"error"`
}

// ErrorDetail describes one failure.
type ErrorDetail struct {
	Code      string       `json:"code"`
	Message   string       `json:"message"`
	RequestID string       `json:"request_id"`
	Details   []FieldError `json:"details,omitempty"`
}

// FieldError points at one invalid input field.
type FieldError struct {
	Field   string `json:"field"`
	Message string `json:"message"`
}

// Page is the envelope returned by every collection endpoint.
type Page[T any] struct {
	Items      []T     `json:"items"`
	NextCursor *string `json:"next_cursor"`
	HasMore    bool    `json:"has_more"`
}

// NewPage builds a Page. An empty nextCursor means this is the last page.
// Items is never null in the encoded output.
func NewPage[T any](items []T, nextCursor string) Page[T] {
	if items == nil {
		items = []T{}
	}
	p := Page[T]{Items: items}
	if nextCursor != "" {
		p.NextCursor = &nextCursor
		p.HasMore = true
	}
	return p
}

// Health is returned by GET /health.
type Health struct {
	Status  string `json:"status"`
	Service string `json:"service"`
	Version string `json:"version"`
}

// Readiness is returned by GET /ready.
type Readiness struct {
	Status  string  `json:"status"`
	Service string  `json:"service"`
	Version string  `json:"version"`
	Checks  []Check `json:"checks"`
}

// Check reports one dependency check. Failure causes stay server-side.
type Check struct {
	Name   string `json:"name"`
	Status string `json:"status"`
}

// Status values used by Health, Readiness and Check.
const (
	StatusOK       = "ok"
	StatusFail     = "fail"
	StatusReady    = "ready"
	StatusNotReady = "not_ready"
)

// LoginRequest is the body of POST /auth/login. It holds a credential and
// must never be logged.
type LoginRequest struct {
	Email    string `json:"email"`
	Password string `json:"password"`
}

// LoginResponse is returned by a successful login.
type LoginResponse struct {
	AccessToken string `json:"access_token"`
	TokenType   string `json:"token_type"`
	// ExpiresIn is the token lifetime in seconds.
	ExpiresIn int64     `json:"expires_in"`
	ExpiresAt time.Time `json:"expires_at"`
	User      User      `json:"user"`
}

// User is the client view of an account. It never includes credentials.
type User struct {
	ID        uuid.UUID `json:"id"`
	Email     string    `json:"email"`
	Role      string    `json:"role"`
	Status    string    `json:"status"`
	CreatedAt time.Time `json:"created_at"`
}

// CreatePatientRequest is the body of POST /patients.
type CreatePatientRequest struct {
	ExternalReference string `json:"external_reference"`
	// DateOfBirth is a calendar date in YYYY-MM-DD form.
	DateOfBirth string `json:"date_of_birth"`
	Sex         string `json:"sex"`
}

// Patient is the client view of a synthetic patient (SPECIFICATIONS.md
// section 11). DateOfBirth is a calendar date in YYYY-MM-DD form.
type Patient struct {
	ID                uuid.UUID `json:"id"`
	ExternalReference string    `json:"external_reference"`
	DateOfBirth       string    `json:"date_of_birth"`
	Sex               string    `json:"sex"`
	Status            string    `json:"status"`
	CreatedAt         time.Time `json:"created_at"`
	UpdatedAt         time.Time `json:"updated_at"`
}

// CreateMeasurementRequest is the body of POST /measurements and one item
// of POST /measurements/batch. Value is a pointer so that an absent value
// is distinguishable from zero. RecordedAt is an RFC 3339 timestamp.
type CreateMeasurementRequest struct {
	PatientID  string          `json:"patient_id"`
	Type       string          `json:"type"`
	Value      *float64        `json:"value"`
	Unit       string          `json:"unit"`
	RecordedAt string          `json:"recorded_at"`
	Source     string          `json:"source"`
	Metadata   json.RawMessage `json:"metadata,omitempty"`
}

// CreateMeasurementBatchRequest is the body of POST /measurements/batch.
type CreateMeasurementBatchRequest struct {
	Items []CreateMeasurementRequest `json:"items"`
}

// Measurement is the client view of one reading (SPECIFICATIONS.md
// section 12).
type Measurement struct {
	ID         uuid.UUID       `json:"id"`
	PatientID  uuid.UUID       `json:"patient_id"`
	Type       string          `json:"type"`
	Value      float64         `json:"value"`
	Unit       string          `json:"unit"`
	RecordedAt time.Time       `json:"recorded_at"`
	CreatedAt  time.Time       `json:"created_at"`
	Source     string          `json:"source"`
	Metadata   json.RawMessage `json:"metadata"`
}

// MeasurementBatch is returned by POST /measurements/batch: the stored
// readings in input order.
type MeasurementBatch struct {
	Items []Measurement `json:"items"`
}
