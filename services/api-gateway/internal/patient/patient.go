// Package patient is the application service behind the Patient API. It
// validates input, applies the rules the specification and OQ-09 set for
// synthetic patients, writes audit records with every state change and
// reports what happened to the observability ports. It knows nothing about
// HTTP or PostgreSQL; the store port below is implemented in infra/postgres.
package patient

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"
	"unicode"

	"github.com/google/uuid"

	"github.com/n0ah-n0wa/VitalMesh/services/api-gateway/internal/auth"
	"github.com/n0ah-n0wa/VitalMesh/services/api-gateway/internal/cache"
	"github.com/n0ah-n0wa/VitalMesh/services/api-gateway/internal/domain"
	"github.com/n0ah-n0wa/VitalMesh/services/api-gateway/internal/observability/metrics"
	"github.com/n0ah-n0wa/VitalMesh/services/api-gateway/internal/observability/tracing"
	"github.com/n0ah-n0wa/VitalMesh/services/api-gateway/internal/pagination"
	"github.com/n0ah-n0wa/VitalMesh/services/api-gateway/internal/requestid"
	"github.com/n0ah-n0wa/VitalMesh/services/api-gateway/internal/validate"
)

// Limits of the patient record, matching the schema constraints.
const (
	MaxExternalReferenceLength = 128
	// ResourceType names patients in audit records.
	ResourceType = "patient"
)

// EarliestDateOfBirth matches the schema constraint on date_of_birth.
var EarliestDateOfBirth = time.Date(1900, 1, 1, 0, 0, 0, 0, time.UTC)

// Error codes of the Patient API.
const (
	CodeNotFound       = "PATIENT_NOT_FOUND"
	CodeAlreadyExists  = "PATIENT_ALREADY_EXISTS"
	CodeAlreadyDeleted = "PATIENT_ALREADY_DELETED"
)

// ErrNotFound is the client-safe error for a patient that does not exist
// or is not visible to the caller.
func ErrNotFound() *domain.Error {
	return domain.New(domain.KindNotFound, CodeNotFound, "The patient does not exist.")
}

// ErrAlreadyExists reports a duplicate external reference.
func ErrAlreadyExists() *domain.Error {
	return domain.New(domain.KindConflict, CodeAlreadyExists, "A patient with that external reference already exists.")
}

// ErrAlreadyDeleted reports a deletion of an already deleted patient.
func ErrAlreadyDeleted() *domain.Error {
	return domain.New(domain.KindConflict, CodeAlreadyDeleted, "The patient is already deleted.")
}

// NewPatient is the validated input of Create.
type NewPatient struct {
	ExternalReference string
	DateOfBirth       time.Time
	Sex               domain.Sex
}

// AuditEvent describes the audit record a state change must be stored
// with, atomically.
type AuditEvent struct {
	ActorID   uuid.UUID
	Action    domain.AuditAction
	RequestID string
}

// Cursor is the keyset position of the patient collection. Its JSON form is
// the opaque cursor clients see.
type Cursor struct {
	CreatedAt time.Time `json:"created_at"`
	ID        uuid.UUID `json:"id"`
}

// Store is the persistence port. Create and SoftDelete must write the
// state change and the audit event in one transaction: either both are
// stored or neither is.
type Store interface {
	Create(ctx context.Context, in NewPatient, event AuditEvent) (domain.Patient, error)
	// GetByID returns the patient whatever its status, or a not-found error.
	GetByID(ctx context.Context, id uuid.UUID) (domain.Patient, error)
	// List returns up to limit non-deleted patients in creation order,
	// after the cursor when one is given.
	List(ctx context.Context, after *Cursor, limit int) ([]domain.Patient, error)
	// SoftDelete marks the patient deleted at now. It returns a not-found
	// error for an unknown id and a conflict for an already deleted one.
	SoftDelete(ctx context.Context, id uuid.UUID, now time.Time, event AuditEvent) (domain.Patient, error)
}

// Page is one page of the collection.
type Page struct {
	Items      []domain.Patient
	NextCursor string
}

// Service implements the Patient API's use cases.
type Service struct {
	store    Store
	cache    cache.Store
	cacheTTL time.Duration
	logger   *slog.Logger
	metrics  metrics.Recorder
	tracer   tracing.Tracer
	now      func() time.Time
}

// Options tune a Service. Nil fields take safe defaults.
type Options struct {
	Metrics metrics.Recorder
	Tracer  tracing.Tracer
	// Cache serves patient reads. Nil means no caching, which is always
	// correct and merely slower.
	Cache cache.Store
	// CacheTTL bounds how long a cached record may be served. Zero
	// disables caching whatever Cache is set to.
	CacheTTL time.Duration
	// Now supplies the current time; nil means time.Now.
	Now func() time.Time
}

// NewService wires a Service.
func NewService(store Store, logger *slog.Logger, opts Options) *Service {
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	cacheStore := opts.Cache
	if cacheStore == nil || opts.CacheTTL <= 0 {
		cacheStore = cache.Disabled{}
	}
	return &Service{
		store:    store,
		cache:    cacheStore,
		cacheTTL: opts.CacheTTL,
		logger:   logger,
		metrics:  metrics.OrNoop(opts.Metrics),
		tracer:   tracing.OrNoop(opts.Tracer),
		now:      now,
	}
}

// CreateInput is the raw input of Create, before validation.
type CreateInput struct {
	ExternalReference string
	// DateOfBirth is a calendar date in YYYY-MM-DD form.
	DateOfBirth string
	Sex         string
}

// Create validates in, stores the patient and records PATIENT_CREATED by
// actor in the same transaction.
func (s *Service) Create(ctx context.Context, actor auth.Principal, in CreateInput) (patient domain.Patient, err error) {
	ctx, finish := s.begin(ctx, "patient.create")
	defer func() { finish(err) }()

	valid, err := s.validate(in)
	if err != nil {
		return domain.Patient{}, err
	}
	patient, err = s.store.Create(ctx, valid, AuditEvent{
		ActorID:   actor.UserID,
		Action:    domain.AuditPatientCreated,
		RequestID: requestid.FromContext(ctx),
	})
	if err != nil {
		return domain.Patient{}, translate(err)
	}
	s.logger.InfoContext(ctx, "patient created", "patient_id", patient.ID, "user_id", actor.UserID)
	return patient, nil
}

// Get returns a patient. A deleted patient is visible to ADMIN only; for
// every other role it does not exist (OQ-09).
//
// The record may come from the short-lived cache (SPECIFICATIONS.md section
// 84). What is cached is the record itself, never the answer to this
// question: the visibility rule below is applied to whatever was read, so
// two callers with different roles cannot be served each other's answer.
// Writes invalidate the entry, and the time to live bounds an invalidation
// that could not be delivered. No decision anywhere else in the gateway
// reads a patient through this path; those go to the database.
func (s *Service) Get(ctx context.Context, actor auth.Principal, id uuid.UUID) (patient domain.Patient, err error) {
	ctx, finish := s.begin(ctx, "patient.get")
	defer func() { finish(err) }()

	key := s.cacheKey(id)
	if s.cache.Get(ctx, key, &patient) && patient.ID == id {
		s.metrics.Cache("patient.get", true)
		return s.visible(actor, patient)
	}
	s.metrics.Cache("patient.get", false)

	patient, err = s.store.GetByID(ctx, id)
	if err != nil {
		return domain.Patient{}, translate(err)
	}
	s.cache.Set(ctx, key, patient, s.cacheTTL)
	return s.visible(actor, patient)
}

// visible applies the deleted-patient rule to a record from either source.
func (s *Service) visible(actor auth.Principal, patient domain.Patient) (domain.Patient, error) {
	if patient.Status == domain.PatientDeleted && actor.Role != domain.RoleAdmin {
		return domain.Patient{}, ErrNotFound()
	}
	return patient, nil
}

// cacheKey is where one patient's cached record lives.
func (s *Service) cacheKey(id uuid.UUID) string {
	return s.cache.Key("patient", id.String())
}

// List returns one page of non-deleted patients in creation order. The
// cursor, when given, must have been produced by a previous page.
func (s *Service) List(ctx context.Context, page pagination.Request) (result Page, err error) {
	ctx, finish := s.begin(ctx, "patient.list")
	defer func() { finish(err) }()

	var after *Cursor
	if page.Cursor != "" {
		var c Cursor
		if err := pagination.DecodeCursor(page.Cursor, &c); err != nil {
			return Page{}, err
		}
		if c.ID == uuid.Nil || c.CreatedAt.IsZero() {
			return Page{}, pagination.ErrInvalidCursor()
		}
		after = &c
	}

	// Ask for one more than the page to learn whether another page exists
	// without a second query.
	items, err := s.store.List(ctx, after, page.Limit+1)
	if err != nil {
		return Page{}, translate(err)
	}
	if len(items) <= page.Limit {
		return Page{Items: items}, nil
	}
	items = items[:page.Limit]
	last := items[len(items)-1]
	next, err := pagination.EncodeCursor(Cursor{CreatedAt: last.CreatedAt, ID: last.ID})
	if err != nil {
		return Page{}, fmt.Errorf("encode cursor: %w", err)
	}
	return Page{Items: items, NextCursor: next}, nil
}

// Delete soft-deletes a patient and records PATIENT_DELETED by actor in
// the same transaction. Deleting twice is a conflict.
func (s *Service) Delete(ctx context.Context, actor auth.Principal, id uuid.UUID) (err error) {
	ctx, finish := s.begin(ctx, "patient.delete")
	defer func() { finish(err) }()

	_, err = s.store.SoftDelete(ctx, id, s.now().UTC(), AuditEvent{
		ActorID:   actor.UserID,
		Action:    domain.AuditPatientDeleted,
		RequestID: requestid.FromContext(ctx),
	})
	if err != nil {
		return translate(err)
	}
	// The record changed, so the cached copy is wrong now, not in thirty
	// seconds. The time to live is the fallback, not the mechanism.
	s.cache.Invalidate(ctx, s.cacheKey(id))
	s.logger.InfoContext(ctx, "patient deleted", "patient_id", id, "user_id", actor.UserID)
	return nil
}

func (s *Service) validate(in CreateInput) (NewPatient, error) {
	var v validate.Validator
	ref := strings.TrimSpace(in.ExternalReference)
	v.Required("external_reference", ref)
	v.MaxLength("external_reference", ref, MaxExternalReferenceLength)
	v.Check(!strings.ContainsFunc(ref, unicode.IsControl), "external_reference", "must not contain control characters")

	sex := domain.Sex(in.Sex)
	v.Required("sex", in.Sex)
	if in.Sex != "" {
		v.OneOf("sex", in.Sex, string(domain.SexFemale), string(domain.SexMale), string(domain.SexOther), string(domain.SexUnknown))
	}

	var dob time.Time
	v.Required("date_of_birth", in.DateOfBirth)
	if in.DateOfBirth != "" {
		parsed, err := time.Parse(time.DateOnly, in.DateOfBirth)
		switch {
		case err != nil:
			v.Add("date_of_birth", "must be a calendar date in YYYY-MM-DD form")
		case parsed.Before(EarliestDateOfBirth):
			v.Add("date_of_birth", "must not be before 1900-01-01")
		case parsed.After(s.now().UTC()):
			v.Add("date_of_birth", "must not be in the future")
		default:
			dob = parsed
		}
	}
	if err := v.Err(); err != nil {
		return NewPatient{}, err
	}
	return NewPatient{ExternalReference: ref, DateOfBirth: dob, Sex: sex}, nil
}

// translate maps store errors onto the Patient API's vocabulary. Errors
// the store already classified precisely pass through.
func translate(err error) error {
	var domErr *domain.Error
	if !errors.As(err, &domErr) {
		return err
	}
	switch {
	case domErr.Kind == domain.KindNotFound:
		return ErrNotFound()
	case domErr.Kind == domain.KindConflict && domErr.Code == "PATIENTS_EXTERNAL_REFERENCE_KEY":
		return ErrAlreadyExists()
	case domErr.Kind == domain.KindConflict && domErr.Code == CodeAlreadyDeleted:
		return ErrAlreadyDeleted()
	}
	return err
}

// begin opens a span and starts the clock for an operation; the returned
// function closes both and records the outcome.
func (s *Service) begin(ctx context.Context, name string) (context.Context, func(error)) {
	start := s.now()
	ctx, span := s.tracer.Start(ctx, name)
	return ctx, func(err error) {
		span.RecordError(err)
		span.End()
		s.metrics.Operation(name, outcomeOf(err), s.now().Sub(start))
	}
}

// outcomeOf classifies an error for metrics without exposing detail.
func outcomeOf(err error) metrics.Outcome {
	if err == nil {
		return metrics.OutcomeOK
	}
	var domErr *domain.Error
	if !errors.As(err, &domErr) {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return metrics.OutcomeCancelled
		}
		return metrics.OutcomeError
	}
	switch domErr.Kind {
	case domain.KindInvalid, domain.KindValidation:
		return metrics.OutcomeInvalid
	case domain.KindNotFound:
		return metrics.OutcomeNotFound
	case domain.KindConflict:
		return metrics.OutcomeConflict
	case domain.KindUnauthorized, domain.KindForbidden:
		return metrics.OutcomeDenied
	case domain.KindTimeout:
		return metrics.OutcomeCancelled
	default:
		return metrics.OutcomeError
	}
}
