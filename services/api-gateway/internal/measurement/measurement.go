package measurement

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"strings"
	"time"
	"unicode"

	"github.com/google/uuid"

	"github.com/n0ah-n0wa/VitalMesh/services/api-gateway/internal/auth"
	"github.com/n0ah-n0wa/VitalMesh/services/api-gateway/internal/config"
	"github.com/n0ah-n0wa/VitalMesh/services/api-gateway/internal/domain"
	"github.com/n0ah-n0wa/VitalMesh/services/api-gateway/internal/observability/metrics"
	"github.com/n0ah-n0wa/VitalMesh/services/api-gateway/internal/observability/tracing"
	"github.com/n0ah-n0wa/VitalMesh/services/api-gateway/internal/pagination"
	"github.com/n0ah-n0wa/VitalMesh/services/api-gateway/internal/requestid"
	"github.com/n0ah-n0wa/VitalMesh/services/api-gateway/internal/validate"
)

// Limits of a reading, matching the schema.
const (
	MaxSourceLength = 64
	// MaxMetadataDepth bounds nesting so that validation and storage stay
	// cheap whatever the client sends.
	MaxMetadataDepth = 8
	// ResourceType names measurements in audit records.
	ResourceType = "measurement"
)

// EarliestRecordedAt is the oldest timestamp accepted.
var EarliestRecordedAt = time.Date(1900, 1, 1, 0, 0, 0, 0, time.UTC)

// Error codes of the Measurement API.
const (
	CodeValidationFailed = "MEASUREMENT_VALIDATION_FAILED"
	CodeNotFound         = "MEASUREMENT_NOT_FOUND"
	CodeAlreadyExists    = "MEASUREMENT_ALREADY_EXISTS"
	CodePatientNotFound  = "PATIENT_NOT_FOUND"
)

const validationMessage = "The measurement payload is invalid."

// ErrNotFound is the client-safe error for a reading that does not exist.
func ErrNotFound() *domain.Error {
	return domain.New(domain.KindNotFound, CodeNotFound, "The measurement does not exist.")
}

// ErrPatientNotFound is the error for listing an unknown or invisible patient.
func ErrPatientNotFound() *domain.Error {
	return domain.New(domain.KindNotFound, CodePatientNotFound, "The patient does not exist.")
}

// ErrAlreadyExists reports a reading equal in patient, type, time and
// source to a stored one. Details name the batch item when known.
func ErrAlreadyExists(details ...domain.FieldError) *domain.Error {
	e := domain.New(domain.KindConflict, CodeAlreadyExists, "An identical measurement was already recorded.")
	e.Details = details
	return e
}

// NewMeasurement is one validated reading.
type NewMeasurement struct {
	PatientID  uuid.UUID
	Type       domain.MeasurementType
	Value      float64
	Unit       string
	RecordedAt time.Time
	Source     string
	// Metadata is a compact JSON object.
	Metadata json.RawMessage
}

// AuditEvent describes the audit record a state change is stored with.
type AuditEvent struct {
	ActorID   uuid.UUID
	Action    domain.AuditAction
	RequestID string
}

// Cursor is the keyset position of a patient's readings, in recording
// order.
type Cursor struct {
	RecordedAt time.Time `json:"recorded_at"`
	ID         uuid.UUID `json:"id"`
}

// Filter narrows a listing. Nil fields are ignored; the time range is
// half-open (From inclusive, To exclusive).
type Filter struct {
	Type *domain.MeasurementType
	From *time.Time
	To   *time.Time
}

// ItemError names the batch item a store rejected.
type ItemError struct {
	Index int
	Err   error
}

func (e *ItemError) Error() string { return fmt.Sprintf("item %d: %v", e.Index, e.Err) }
func (e *ItemError) Unwrap() error { return e.Err }

// Store is the persistence port. Writes carry the audit event that must be
// stored in the same transaction as the change; a batch is all-or-nothing
// and a rejected batch item is reported as an *ItemError.
type Store interface {
	Create(ctx context.Context, in NewMeasurement, event AuditEvent) (domain.Measurement, error)
	CreateBatch(ctx context.Context, in []NewMeasurement, event AuditEvent) ([]domain.Measurement, error)
	GetByID(ctx context.Context, id uuid.UUID) (domain.Measurement, error)
	ListByPatient(ctx context.Context, patientID uuid.UUID, filter Filter, after *Cursor, limit int) ([]domain.Measurement, error)
	Delete(ctx context.Context, id uuid.UUID, event AuditEvent) error
	// PatientStatus returns the status of a patient, or a not-found error.
	PatientStatus(ctx context.Context, id uuid.UUID) (domain.PatientStatus, error)
}

// Page is one page of a patient's readings.
type Page struct {
	Items      []domain.Measurement
	NextCursor string
}

// Service implements the Measurement API's use cases.
type Service struct {
	store   Store
	limits  config.Measurements
	logger  *slog.Logger
	metrics metrics.Recorder
	tracer  tracing.Tracer
	now     func() time.Time
}

// Options tune a Service. Nil fields take safe defaults.
type Options struct {
	Metrics metrics.Recorder
	Tracer  tracing.Tracer
	Now     func() time.Time
}

// NewService wires a Service with the configured ingestion limits.
func NewService(store Store, limits config.Measurements, logger *slog.Logger, opts Options) *Service {
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	return &Service{
		store:   store,
		limits:  limits,
		logger:  logger,
		metrics: metrics.OrNoop(opts.Metrics),
		tracer:  tracing.OrNoop(opts.Tracer),
		now:     now,
	}
}

// Input is one reading as the client sent it, before validation. Value is
// a pointer so that an absent value is distinguishable from zero.
type Input struct {
	PatientID  string
	Type       string
	Value      *float64
	Unit       string
	RecordedAt string
	Source     string
	Metadata   json.RawMessage
}

// Create validates and stores one reading with a MEASUREMENT_CREATED
// audit record by actor.
func (s *Service) Create(ctx context.Context, actor auth.Principal, in Input) (m domain.Measurement, err error) {
	ctx, finish := s.begin(ctx, "measurement.create")
	defer func() { finish(err) }()

	v := s.validator()
	valid := s.validateOne(&v, in)
	if err := v.Err(); err != nil {
		return domain.Measurement{}, err
	}
	if err := s.checkPatients(ctx, &v, map[uuid.UUID][]string{valid.PatientID: {"patient_id"}}); err != nil {
		return domain.Measurement{}, err
	}
	m, err = s.store.Create(ctx, valid, s.event(ctx, actor, domain.AuditMeasurementCreated))
	if err != nil {
		return domain.Measurement{}, s.storeError(ctx, err)
	}
	s.logger.InfoContext(ctx, "measurement created", "measurement_id", m.ID, "patient_id", m.PatientID, "type", m.Type, "user_id", actor.UserID)
	return m, nil
}

// CreateBatch validates every reading and stores all of them or none, with
// one MEASUREMENT_CREATED audit record per reading. Every invalid item is
// reported, by index, in one validation error.
func (s *Service) CreateBatch(ctx context.Context, actor auth.Principal, items []Input) (ms []domain.Measurement, err error) {
	ctx, finish := s.begin(ctx, "measurement.batch")
	defer func() { finish(err) }()
	s.metrics.Batch("measurement.batch", len(items))

	v := s.validator()
	v.Check(len(items) > 0, "items", "must contain at least one measurement")
	v.Check(len(items) <= s.limits.MaxBatchSize, "items", fmt.Sprintf("must contain at most %d measurements", s.limits.MaxBatchSize))
	if err := v.Err(); err != nil {
		return nil, err
	}

	valid := make([]NewMeasurement, len(items))
	patients := map[uuid.UUID][]string{}
	seen := map[readingKey]int{}
	for i, in := range items {
		item := s.validator()
		item.Prefix = fmt.Sprintf("items[%d].", i)
		valid[i] = s.validateOne(&item, in)
		if item.Valid() {
			field := fmt.Sprintf("items[%d].patient_id", i)
			patients[valid[i].PatientID] = append(patients[valid[i].PatientID], field)
			k := keyOf(valid[i])
			if first, dup := seen[k]; dup {
				v.Add(fmt.Sprintf("items[%d]", i), fmt.Sprintf("duplicates items[%d] (same patient, type, recorded_at and source)", first))
			} else {
				seen[k] = i
			}
		}
		v.Merge(&item)
	}
	// Patient checks run even when other items are invalid, so one
	// response names every problem in the batch.
	if err := s.checkPatients(ctx, &v, patients); err != nil {
		return nil, err
	}

	ms, err = s.store.CreateBatch(ctx, valid, s.event(ctx, actor, domain.AuditMeasurementCreated))
	if err != nil {
		return nil, s.storeError(ctx, err)
	}
	s.logger.InfoContext(ctx, "measurement batch created", "count", len(ms), "user_id", actor.UserID)
	return ms, nil
}

// Get returns one reading. Readings of a deleted patient follow the
// patient's visibility: ADMIN sees them, every other role does not.
func (s *Service) Get(ctx context.Context, actor auth.Principal, id uuid.UUID) (m domain.Measurement, err error) {
	ctx, finish := s.begin(ctx, "measurement.get")
	defer func() { finish(err) }()
	m, err = s.store.GetByID(ctx, id)
	if err != nil {
		return domain.Measurement{}, translate(err)
	}
	if err := s.visible(ctx, actor, m); err != nil {
		return domain.Measurement{}, err
	}
	return m, nil
}

// visible reports ErrNotFound when m belongs to a deleted patient and actor
// is not ADMIN, mirroring the patient's own visibility (OQ-09).
func (s *Service) visible(ctx context.Context, actor auth.Principal, m domain.Measurement) error {
	if actor.Role == domain.RoleAdmin {
		return nil
	}
	status, err := s.store.PatientStatus(ctx, m.PatientID)
	if err != nil {
		if isNotFound(err) {
			return ErrNotFound()
		}
		return err
	}
	if status == domain.PatientDeleted {
		return ErrNotFound()
	}
	return nil
}

// ListInput is the raw input of ListByPatient.
type ListInput struct {
	Type string
	From string
	To   string
	Page pagination.Request
}

// ListByPatient returns one page of a patient's readings in recording
// order. A deleted patient is visible to ADMIN only (as for the patient
// itself).
func (s *Service) ListByPatient(ctx context.Context, actor auth.Principal, patientID uuid.UUID, in ListInput) (page Page, err error) {
	ctx, finish := s.begin(ctx, "measurement.list")
	defer func() { finish(err) }()

	filter, after, err := s.validateList(in)
	if err != nil {
		return Page{}, err
	}
	status, err := s.store.PatientStatus(ctx, patientID)
	if err != nil {
		if isNotFound(err) {
			return Page{}, ErrPatientNotFound()
		}
		return Page{}, err
	}
	if status == domain.PatientDeleted && actor.Role != domain.RoleAdmin {
		return Page{}, ErrPatientNotFound()
	}

	items, err := s.store.ListByPatient(ctx, patientID, filter, after, in.Page.Limit+1)
	if err != nil {
		return Page{}, translate(err)
	}
	if len(items) <= in.Page.Limit {
		return Page{Items: items}, nil
	}
	items = items[:in.Page.Limit]
	last := items[len(items)-1]
	next, err := pagination.EncodeCursor(Cursor{RecordedAt: last.RecordedAt, ID: last.ID})
	if err != nil {
		return Page{}, fmt.Errorf("encode cursor: %w", err)
	}
	return Page{Items: items, NextCursor: next}, nil
}

// Delete removes a reading and records MEASUREMENT_DELETED by actor in the
// same transaction.
func (s *Service) Delete(ctx context.Context, actor auth.Principal, id uuid.UUID) (err error) {
	ctx, finish := s.begin(ctx, "measurement.delete")
	defer func() { finish(err) }()
	m, err := s.store.GetByID(ctx, id)
	if err != nil {
		return translate(err)
	}
	if err := s.visible(ctx, actor, m); err != nil {
		return err
	}
	if err := s.store.Delete(ctx, id, s.event(ctx, actor, domain.AuditMeasurementDeleted)); err != nil {
		return translate(err)
	}
	s.logger.InfoContext(ctx, "measurement deleted", "measurement_id", id, "user_id", actor.UserID)
	return nil
}

func (s *Service) validator() validate.Validator {
	return validate.Validator{Code: CodeValidationFailed, Message: validationMessage}
}

func (s *Service) event(ctx context.Context, actor auth.Principal, action domain.AuditAction) AuditEvent {
	return AuditEvent{ActorID: actor.UserID, Action: action, RequestID: requestid.FromContext(ctx)}
}

// validateOne checks every field of in and returns the normalised reading.
// Failures are recorded on v; the returned value is meaningful only when v
// stays valid.
func (s *Service) validateOne(v *validate.Validator, in Input) NewMeasurement {
	var out NewMeasurement

	v.Required("patient_id", in.PatientID)
	if in.PatientID != "" {
		id, err := uuid.Parse(in.PatientID)
		v.Check(err == nil && id != uuid.Nil, "patient_id", "must be a UUID")
		out.PatientID = id
	}

	v.Required("type", in.Type)
	typ, known := Lookup(domain.MeasurementType(in.Type))
	if in.Type != "" {
		v.Check(known, "type", "must be one of: "+strings.Join(TypeCodes(), ", "))
		out.Type = typ.Code
	}

	switch {
	case in.Value == nil:
		v.Add("value", "is required")
	case math.IsNaN(*in.Value) || math.IsInf(*in.Value, 0):
		v.Add("value", "must be a finite number")
	case known && (*in.Value < typ.Min || *in.Value > typ.Max):
		v.Add("value", fmt.Sprintf("must be between %g and %g %s for %s", typ.Min, typ.Max, typ.Unit, typ.Code))
	default:
		out.Value = *in.Value
	}

	v.Required("unit", in.Unit)
	if in.Unit != "" && known {
		v.Check(in.Unit == typ.Unit, "unit", fmt.Sprintf("must be %q for %s", typ.Unit, typ.Code))
	}
	out.Unit = in.Unit

	v.Required("recorded_at", in.RecordedAt)
	if in.RecordedAt != "" {
		t, err := time.Parse(time.RFC3339Nano, in.RecordedAt)
		switch {
		case err != nil:
			v.Add("recorded_at", "must be an RFC 3339 timestamp with a UTC offset")
		case t.Before(EarliestRecordedAt):
			v.Add("recorded_at", "must not be before 1900-01-01T00:00:00Z")
		case t.After(s.now().Add(s.limits.MaxFutureSkew)):
			v.Add("recorded_at", fmt.Sprintf("must not be more than %s in the future", s.limits.MaxFutureSkew))
		default:
			// PostgreSQL stores microseconds; normalise here so that the
			// duplicate rule and the returned value match what is stored.
			out.RecordedAt = t.UTC().Truncate(time.Microsecond)
		}
	}

	source := strings.TrimSpace(in.Source)
	v.Required("source", source)
	v.MaxLength("source", source, MaxSourceLength)
	v.Check(!strings.ContainsFunc(source, unicode.IsControl), "source", "must not contain control characters")
	out.Source = source

	out.Metadata = s.validateMetadata(v, in.Metadata)
	return out
}

// validateMetadata accepts an absent or null metadata, or a JSON object
// within the size and depth limits, and returns its compact form.
func (s *Service) validateMetadata(v *validate.Validator, raw json.RawMessage) json.RawMessage {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return json.RawMessage(`{}`)
	}
	if !json.Valid(trimmed) {
		v.Add("metadata", "must be valid JSON")
		return nil
	}
	if trimmed[0] != '{' {
		v.Add("metadata", "must be a JSON object")
		return nil
	}
	if depth := jsonDepth(trimmed); depth > MaxMetadataDepth {
		v.Add("metadata", fmt.Sprintf("must not nest deeper than %d levels", MaxMetadataDepth))
		return nil
	}
	var compact bytes.Buffer
	if err := json.Compact(&compact, trimmed); err != nil {
		v.Add("metadata", "must be valid JSON")
		return nil
	}
	if compact.Len() > s.limits.MaxMetadataBytes {
		v.Add("metadata", fmt.Sprintf("must be at most %d bytes as compact JSON", s.limits.MaxMetadataBytes))
		return nil
	}
	return json.RawMessage(compact.Bytes())
}

// jsonDepth returns the deepest nesting of arrays and objects in valid JSON.
func jsonDepth(data []byte) int {
	dec := json.NewDecoder(bytes.NewReader(data))
	depth, deepest := 0, 0
	for {
		tok, err := dec.Token()
		if err != nil {
			return deepest
		}
		if d, ok := tok.(json.Delim); ok {
			switch d {
			case '{', '[':
				depth++
				if depth > deepest {
					deepest = depth
				}
			case '}', ']':
				depth--
			}
		}
	}
}

func (s *Service) validateList(in ListInput) (Filter, *Cursor, error) {
	v := s.validator()
	var filter Filter
	if in.Type != "" {
		typ, ok := Lookup(domain.MeasurementType(in.Type))
		v.Check(ok, "type", "must be one of: "+strings.Join(TypeCodes(), ", "))
		if ok {
			filter.Type = &typ.Code
		}
	}
	parse := func(field, value string) *time.Time {
		if value == "" {
			return nil
		}
		t, err := time.Parse(time.RFC3339Nano, value)
		if err != nil {
			v.Add(field, "must be an RFC 3339 timestamp with a UTC offset")
			return nil
		}
		t = t.UTC().Truncate(time.Microsecond)
		return &t
	}
	filter.From = parse("from", in.From)
	filter.To = parse("to", in.To)
	if filter.From != nil && filter.To != nil && !filter.From.Before(*filter.To) {
		v.Add("to", "must be after from")
	}
	var after *Cursor
	if in.Page.Cursor != "" {
		var c Cursor
		if err := pagination.DecodeCursor(in.Page.Cursor, &c); err != nil {
			return Filter{}, nil, err
		}
		if c.ID == uuid.Nil || c.RecordedAt.IsZero() {
			return Filter{}, nil, pagination.ErrInvalidCursor()
		}
		after = &c
	}
	if err := v.Err(); err != nil {
		return Filter{}, nil, err
	}
	return filter, after, nil
}

// checkPatients verifies that every referenced patient exists and is not
// deleted, reporting each failure on the fields that referenced it.
func (s *Service) checkPatients(ctx context.Context, v *validate.Validator, patients map[uuid.UUID][]string) error {
	for id, fields := range patients {
		status, err := s.store.PatientStatus(ctx, id)
		switch {
		case err != nil && isNotFound(err):
			for _, f := range fields {
				v.Add(f, "does not reference an existing patient")
			}
		case err != nil:
			return err
		case status == domain.PatientDeleted:
			for _, f := range fields {
				v.Add(f, "references a deleted patient")
			}
		}
	}
	return v.Err()
}

type readingKey struct {
	patient    uuid.UUID
	typ        domain.MeasurementType
	recordedAt time.Time
	source     string
}

func keyOf(m NewMeasurement) readingKey {
	return readingKey{m.PatientID, m.Type, m.RecordedAt, m.Source}
}

func isNotFound(err error) bool {
	var domErr *domain.Error
	return errors.As(err, &domErr) && domErr.Kind == domain.KindNotFound
}

// translate maps store errors onto the Measurement API's vocabulary.
func translate(err error) error {
	var item *ItemError
	if errors.As(err, &item) {
		inner := translate(item.Err)
		var domErr *domain.Error
		if errors.As(inner, &domErr) {
			field := fmt.Sprintf("items[%d]", item.Index)
			switch domErr.Kind {
			case domain.KindConflict:
				return ErrAlreadyExists(domain.FieldError{Field: field, Message: "was already recorded"})
			case domain.KindValidation:
				e := &domain.Error{Kind: domain.KindValidation, Code: CodeValidationFailed, Message: validationMessage,
					Details: []domain.FieldError{{Field: field, Message: "was rejected by a data constraint"}}, Err: err}
				return e
			}
		}
		return inner
	}
	var domErr *domain.Error
	if !errors.As(err, &domErr) {
		return err
	}
	switch {
	case domErr.Kind == domain.KindNotFound:
		return ErrNotFound()
	case domErr.Kind == domain.KindConflict && domErr.Code == "MEASUREMENTS_PATIENT_TYPE_TIME_SOURCE_KEY":
		return ErrAlreadyExists()
	case domErr.Kind == domain.KindValidation:
		// A schema rule the service did not anticipate (for instance a
		// catalogue drift); report it as validation without the constraint.
		return &domain.Error{Kind: domain.KindValidation, Code: CodeValidationFailed, Message: validationMessage,
			Details: []domain.FieldError{{Field: "measurement", Message: "was rejected by a data constraint"}}, Err: err}
	}
	return err
}

// storeError translates a write failure and logs the cases that mean the
// service and the schema disagree, which respond.Error would otherwise
// keep silent because the client-facing error is not internal.
func (s *Service) storeError(ctx context.Context, err error) error {
	translated := translate(err)
	var domErr *domain.Error
	if errors.As(translated, &domErr) && domErr.Kind == domain.KindValidation && domErr.Err != nil {
		s.logger.WarnContext(ctx, "database rejected a validated measurement", "error", domErr.Err)
	}
	return translated
}

func (s *Service) begin(ctx context.Context, name string) (context.Context, func(error)) {
	start := s.now()
	ctx, span := s.tracer.Start(ctx, name)
	return ctx, func(err error) {
		span.RecordError(err)
		span.End()
		s.metrics.Operation(name, outcomeOf(err), s.now().Sub(start))
	}
}

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
