// Package processing implements the Processing API (SPECIFICATIONS.md
// section 13) and the gateway's half of the internal contract with the Rust
// processor (section 8).
//
// The gateway owns the job: it validates the request, records the job, reads
// the measurements, dispatches them to the processor, and records what came
// back. The processor holds no database connection, so every measurement it
// sees travels in the request body.
//
// # Transaction boundaries
//
// Three short transactions bracket one long external call, and no
// transaction is open while that call is in flight (SPECIFICATIONS.md
// section 22):
//
//  1. create the job PENDING with its audit record;
//  2. move it to PROCESSING, counting the attempt;
//     — read the measurements, then call the processor, outside any
//     transaction —
//  3. store the results and move the job to COMPLETED, or record why it
//     FAILED.
//
// Each transition is a compare-and-set on the current status and the
// database rejects transitions outside the state machine, so a job cannot
// be moved twice or moved backwards.
//
// # Failure
//
// Every failure leaves the job in a terminal state with a stable code and a
// safe message (section 94), and is classified as retryable or not (section
// 93). A processor that is unavailable, overloaded or slow is retryable and
// is retried a bounded number of times within the caller's deadline; the
// client then receives a 5xx, which leaves no idempotency record, so the
// whole request can be retried. Anything the processor refuses outright is
// not retried.
package processing

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/n0ah-n0wa/VitalMesh/services/api-gateway/internal/auth"
	"github.com/n0ah-n0wa/VitalMesh/services/api-gateway/internal/config"
	"github.com/n0ah-n0wa/VitalMesh/services/api-gateway/internal/domain"
	"github.com/n0ah-n0wa/VitalMesh/services/api-gateway/internal/measurement"
	"github.com/n0ah-n0wa/VitalMesh/services/api-gateway/internal/observability/metrics"
	"github.com/n0ah-n0wa/VitalMesh/services/api-gateway/internal/observability/tracing"
	"github.com/n0ah-n0wa/VitalMesh/services/api-gateway/internal/pagination"
	"github.com/n0ah-n0wa/VitalMesh/services/api-gateway/internal/requestid"
	"github.com/n0ah-n0wa/VitalMesh/services/api-gateway/internal/validate"
)

// ResourceType names processing jobs in audit records.
const ResourceType = "processing_job"

// Error codes of the Processing API. They are stable and are what a client
// branches on.
const (
	CodeValidationFailed = "PROCESSING_VALIDATION_FAILED"
	CodeJobNotFound      = "PROCESSING_JOB_NOT_FOUND"
	CodePatientNotFound  = "PATIENT_NOT_FOUND"
	// CodeNoMeasurements means the requested range holds nothing to process.
	CodeNoMeasurements = "PROCESSING_NO_MEASUREMENTS"
	// CodeJobTooLarge means the range holds more readings than one job may
	// carry.
	CodeJobTooLarge = "PROCESSING_JOB_TOO_LARGE"
	// CodeProcessorUnavailable means the processor could not be reached or
	// had no capacity, after the configured attempts.
	CodeProcessorUnavailable = "PROCESSOR_UNAVAILABLE"
	// CodeProcessorTimeout means the processor did not answer in time.
	CodeProcessorTimeout = "PROCESSING_TIMEOUT"
	// CodeProcessorRejected means the processor refused the job for a reason
	// the data explains.
	CodeProcessorRejected = "PROCESSING_REJECTED"
	// CodeProcessorBusy means the processor is still working on an earlier
	// attempt at this job, which happens when a previous attempt timed out
	// here but not there. It is transient.
	CodeProcessorBusy = "PROCESSOR_BUSY"
	// CodeProcessingIncomplete means the processor did not process every
	// reading it was sent, so its statistics cover less data than the job
	// asked for. Reporting that job as complete would present a partial
	// answer as a whole one.
	CodeProcessingIncomplete = "PROCESSING_INCOMPLETE"
	// CodeProcessorProtocol means the processor answered something this
	// gateway cannot use: a malformed body, or a failure that says the two
	// services disagree. It never carries the processor's own words.
	CodeProcessorProtocol = "PROCESSOR_PROTOCOL_ERROR"
)

const validationMessage = "The processing request is invalid."

// ErrJobNotFound is the client-safe error for a job that does not exist.
func ErrJobNotFound() *domain.Error {
	return domain.New(domain.KindNotFound, CodeJobNotFound, "The processing job does not exist.")
}

// ErrPatientNotFound is the error for an unknown or invisible patient.
func ErrPatientNotFound() *domain.Error {
	return domain.New(domain.KindNotFound, CodePatientNotFound, "The patient does not exist.")
}

// Windows are the aggregation windows the processor supports
// (SPECIFICATIONS.md section 15). The order is the one results are reported
// in.
var Windows = []string{"1m", "5m", "15m", "1h", "6h", "24h", "7d"}

// MaxPercentiles bounds how many percentile ranks one job may request.
const MaxPercentiles = 20

// Input is a job request as the client sent it, before validation.
type Input struct {
	PatientID        string
	MeasurementTypes []string
	Windows          []string
	Percentiles      []int
	// From and To bound which readings are processed, half-open and
	// optional. Empty means unbounded on that side.
	From string
	To   string
}

// Parameters are the validated processing parameters. They are stored with
// the job and sent to the processor unchanged, so a result can always be
// traced to what was asked for.
type Parameters struct {
	MeasurementTypes []domain.MeasurementType `json:"measurement_types"`
	Windows          []string                 `json:"windows"`
	Percentiles      []int                    `json:"percentiles"`
	From             *time.Time               `json:"from,omitempty"`
	To               *time.Time               `json:"to,omitempty"`
}

// NewJob is a validated job request.
type NewJob struct {
	PatientID        uuid.UUID
	Parameters       Parameters
	AlgorithmVersion string
	CreatedBy        uuid.UUID
	RequestID        string
	CorrelationID    string
}

// AuditEvent describes the audit record a state change is stored with.
type AuditEvent struct {
	ActorID   uuid.UUID
	Action    domain.AuditAction
	RequestID string
}

// Reading is one measurement in the shape the processor accepts. The
// gateway builds these from stored measurements; the field names are the
// contract's.
type Reading struct {
	ID         string  `json:"id"`
	Type       string  `json:"type"`
	Value      float64 `json:"value"`
	Unit       string  `json:"unit"`
	RecordedAt string  `json:"recorded_at"`
}

// Request is one dispatch to the processor, matching the contract's
// ProcessRequest.
type Request struct {
	Job      RequestJob `json:"job"`
	Readings []Reading  `json:"readings"`
}

// RequestJob is the contract's ProcessingJob.
type RequestJob struct {
	ID               string           `json:"id"`
	PatientID        string           `json:"patient_id"`
	Parameters       RequestParams    `json:"parameters"`
	AlgorithmVersion string           `json:"algorithm_version"`
	Status           domain.JobStatus `json:"status"`
	RequestedAt      string           `json:"requested_at"`
}

// RequestParams is the contract's ProcessingParameters. The gateway's own
// From and To are not part of it: they select which readings to send, and
// the processor only ever sees the readings themselves.
type RequestParams struct {
	MeasurementTypes []string `json:"measurement_types"`
	Windows          []string `json:"windows"`
	Percentiles      []int    `json:"percentiles"`
}

// Outcome is what the processor returned, matching the contract's
// ProcessOutcome.
type Outcome struct {
	JobID            string   `json:"job_id"`
	AlgorithmVersion string   `json:"algorithm_version"`
	ServiceVersion   string   `json:"service_version"`
	Accepted         int      `json:"accepted"`
	Skipped          int      `json:"skipped"`
	Rejected         []any    `json:"rejected"`
	Results          []Result `json:"results"`
}

// Result is one entry of the outcome, matching the contract's
// ProcessingResult.
type Result struct {
	JobID            string          `json:"job_id"`
	MeasurementType  string          `json:"measurement_type"`
	Window           string          `json:"window"`
	WindowStart      time.Time       `json:"window_start"`
	Statistics       json.RawMessage `json:"statistics"`
	Anomalies        json.RawMessage `json:"anomalies"`
	AlgorithmVersion string          `json:"algorithm_version"`
	ServiceVersion   string          `json:"service_version"`
}

// StoredResult is one result on its way into the database.
type StoredResult struct {
	MeasurementType  domain.MeasurementType
	Window           string
	WindowStart      time.Time
	Statistics       json.RawMessage
	Anomalies        json.RawMessage
	AlgorithmVersion string
	ServiceVersion   string
}

// Failure is why a job ended without results.
type Failure struct {
	Code    string
	Message string
}

// Page is one page of a patient's results.
type Page struct {
	Items      []domain.ProcessingResult
	NextCursor string
}

// ResultCursor is the keyset position of a patient's results.
type ResultCursor struct {
	MeasurementType domain.MeasurementType `json:"measurement_type"`
	Window          string                 `json:"window"`
	WindowStart     time.Time              `json:"window_start"`
	JobID           uuid.UUID              `json:"job_id"`
}

// Store is the persistence port. Each method is one transaction, or one
// read; none of them performs an external call, so no transaction is ever
// open while the processor is being called.
type Store interface {
	// CreateJob inserts a PENDING job and its audit record atomically.
	CreateJob(ctx context.Context, in NewJob, event AuditEvent) (domain.ProcessingJob, error)
	// StartJob moves a PENDING job to PROCESSING, counting the attempt.
	StartJob(ctx context.Context, id uuid.UUID, now time.Time, serviceVersion string) (domain.ProcessingJob, error)
	// CompleteJob stores the results and moves the job to COMPLETED
	// atomically: a job is never COMPLETED without its results.
	CompleteJob(ctx context.Context, id uuid.UUID, now time.Time, results []StoredResult, serviceVersion string) (domain.ProcessingJob, error)
	// FailJob moves a PROCESSING job to FAILED with its diagnosis.
	FailJob(ctx context.Context, id uuid.UUID, now time.Time, failure Failure) (domain.ProcessingJob, error)
	// GetJob returns the job or a not-found error.
	GetJob(ctx context.Context, id uuid.UUID) (domain.ProcessingJob, error)
	// ReadingsForJob returns up to limit readings of the patient in the
	// range, in recording order. It returns limit+1 items when more exist,
	// so the caller can detect an oversized job without a second query.
	ReadingsForJob(ctx context.Context, patientID uuid.UUID, params Parameters, limit int) ([]domain.Measurement, error)
	// ListResultsByPatient returns a page of a patient's results.
	ListResultsByPatient(ctx context.Context, patientID uuid.UUID, after *ResultCursor, limit int) ([]domain.ProcessingResult, error)
	// PatientStatus returns the status of a patient, or a not-found error.
	PatientStatus(ctx context.Context, id uuid.UUID) (domain.PatientStatus, error)
}

// Processor is the port onto the Rust service. Implementations own their
// own timeout, retries and retry classification; the service only cares
// whether the call succeeded and, if not, how the failure is classified.
type Processor interface {
	// Process dispatches one job. The returned error, when the call failed,
	// is a *ProcessorError.
	Process(ctx context.Context, req Request) (Outcome, error)
}

// ProcessorError is a failed call to the processor, already classified.
type ProcessorError struct {
	// Code is the gateway-side code the job is recorded and answered with.
	Code string
	// Message is safe to return to clients. It never contains the
	// processor's own message, which is written for operators of that
	// service.
	Message string
	// Kind is how the failure is answered to the client.
	Kind domain.Kind
	// Retryable says whether the call was worth repeating. It is recorded
	// for diagnosis; by the time the service sees the error, the retries
	// the client's deadline allowed have already been spent.
	Retryable bool
	// Err is the underlying cause, for logs only.
	Err error
}

func (e *ProcessorError) Error() string {
	if e.Err != nil {
		return fmt.Sprintf("%s: %v", e.Code, e.Err)
	}
	return e.Code
}

func (e *ProcessorError) Unwrap() error { return e.Err }

// Service implements the Processing API's use cases.
type Service struct {
	store     Store
	processor Processor
	limits    config.Processing
	logger    *slog.Logger
	metrics   metrics.Recorder
	tracer    tracing.Tracer
	now       func() time.Time
}

// Options tune a Service. Nil fields take safe defaults.
type Options struct {
	Metrics metrics.Recorder
	Tracer  tracing.Tracer
	Now     func() time.Time
}

// NewService wires a Service.
func NewService(store Store, processor Processor, limits config.Processing, logger *slog.Logger, opts Options) *Service {
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	return &Service{
		store:     store,
		processor: processor,
		limits:    limits,
		logger:    logger,
		metrics:   metrics.OrNoop(opts.Metrics),
		tracer:    tracing.OrNoop(opts.Tracer),
		now:       now,
	}
}

// Create validates the request, records the job, dispatches it and records
// the outcome. It returns the job in its terminal state.
//
// The job row survives every outcome: a job that failed is FAILED with its
// code, never absent (SPECIFICATIONS.md section 94).
func (s *Service) Create(ctx context.Context, actor auth.Principal, in Input) (job domain.ProcessingJob, err error) {
	ctx, finish := s.begin(ctx, "processing.create")
	defer func() { finish(err) }()

	valid, err := s.validate(in)
	if err != nil {
		return domain.ProcessingJob{}, err
	}
	if err := s.patientVisible(ctx, actor, valid.PatientID); err != nil {
		return domain.ProcessingJob{}, err
	}

	valid.AlgorithmVersion = s.limits.AlgorithmVersion
	valid.CreatedBy = actor.UserID
	valid.RequestID = requestid.FromContext(ctx)
	valid.CorrelationID = requestid.CorrelationFromContext(ctx)
	if valid.CorrelationID == "" {
		valid.CorrelationID = valid.RequestID
	}

	// (1) The job exists before anything else happens, so a crash between
	// here and the dispatch leaves a PENDING job rather than nothing.
	job, err = s.store.CreateJob(ctx, valid, AuditEvent{
		ActorID:   actor.UserID,
		Action:    domain.AuditProcessingJobCreated,
		RequestID: valid.RequestID,
	})
	if err != nil {
		return domain.ProcessingJob{}, s.storeError(ctx, err)
	}
	s.logger.InfoContext(ctx, "processing job created",
		"job_id", job.ID, "patient_id", job.PatientID, "user_id", actor.UserID)

	return s.run(ctx, job, valid)
}

// run reads the measurements, dispatches them and records the outcome. The
// job is already PENDING; whatever happens from here, it ends in a terminal
// state.
func (s *Service) run(ctx context.Context, job domain.ProcessingJob, valid NewJob) (domain.ProcessingJob, error) {
	// Reading the measurements is a plain query outside any transaction:
	// nothing is being changed and the processor call must not wait on a
	// lock. One extra row is asked for so an oversized job is detected here
	// rather than by the processor.
	readings, err := s.store.ReadingsForJob(ctx, valid.PatientID, valid.Parameters, s.limits.MaxJobMeasurements+1)
	if err != nil {
		return domain.ProcessingJob{}, s.abandon(ctx, job, s.storeError(ctx, err))
	}
	switch {
	case len(readings) == 0:
		return domain.ProcessingJob{}, s.abandon(ctx, job, domain.New(domain.KindValidation, CodeNoMeasurements,
			"The requested range holds no measurements to process."))
	case len(readings) > s.limits.MaxJobMeasurements:
		e := domain.New(domain.KindValidation, CodeJobTooLarge,
			fmt.Sprintf("The requested range holds more than %d measurements; narrow it with from and to.", s.limits.MaxJobMeasurements))
		return domain.ProcessingJob{}, s.abandon(ctx, job, e)
	}

	// (2) The job is PROCESSING before the call, so its state is honest
	// while the call is in flight and the attempt is counted even if this
	// process dies.
	started, err := s.store.StartJob(ctx, job.ID, s.now(), s.limits.ServiceVersion)
	if err != nil {
		return domain.ProcessingJob{}, s.storeError(ctx, err)
	}

	// The external call. No transaction is open here.
	outcome, callErr := s.processor.Process(ctx, s.request(started, valid, readings))
	if callErr != nil {
		return domain.ProcessingJob{}, s.recordFailure(ctx, started, callErr)
	}

	results, err := s.storedResults(outcome, len(readings))
	if err != nil {
		return domain.ProcessingJob{}, s.recordFailure(ctx, started, err)
	}

	// (3) Results and the transition are one transaction: a COMPLETED job
	// always has its results, and a job with results is always COMPLETED.
	completed, err := s.store.CompleteJob(ctx, started.ID, s.now(), results, outcome.ServiceVersion)
	if err != nil {
		return domain.ProcessingJob{}, s.storeError(ctx, err)
	}
	s.metrics.Batch("processing.results", len(results))
	s.logger.InfoContext(ctx, "processing job completed",
		"job_id", completed.ID,
		"patient_id", completed.PatientID,
		"readings", len(readings),
		"accepted", outcome.Accepted,
		"skipped", outcome.Skipped,
		"rejected", len(outcome.Rejected),
		"results", len(results),
		"attempt_count", completed.AttemptCount,
		"service_version", outcome.ServiceVersion)
	return completed, nil
}

// abandon fails a job that cannot be dispatched at all, so that the record
// left behind says why, and returns the client-facing error.
func (s *Service) abandon(ctx context.Context, job domain.ProcessingJob, cause error) error {
	failure := Failure{Code: CodeProcessorProtocol, Message: "The job could not be prepared."}
	var domErr *domain.Error
	if errors.As(cause, &domErr) {
		failure = Failure{Code: domErr.Code, Message: domErr.Message}
	}
	// The job is PENDING; the state machine only allows FAILED from
	// PROCESSING, so it is started first. The attempt is real: this gateway
	// did take the job up.
	started, err := s.store.StartJob(ctx, job.ID, s.now(), s.limits.ServiceVersion)
	if err != nil {
		s.logger.ErrorContext(ctx, "job could not be started to record its failure",
			"job_id", job.ID, "error", err)
		return cause
	}
	s.failQuietly(ctx, started, failure)
	return cause
}

// recordFailure marks the job FAILED and returns the error the client sees.
func (s *Service) recordFailure(ctx context.Context, job domain.ProcessingJob, cause error) error {
	var pe *ProcessorError
	if !errors.As(cause, &pe) {
		pe = &ProcessorError{
			Code:    CodeProcessorProtocol,
			Message: "The processing service returned something this gateway cannot use.",
			Kind:    domain.KindUnavailable,
			Err:     cause,
		}
	}
	s.failQuietly(ctx, job, Failure{Code: pe.Code, Message: pe.Message})
	s.logger.ErrorContext(ctx, "processing job failed",
		"job_id", job.ID,
		"patient_id", job.PatientID,
		"code", pe.Code,
		"retryable", pe.Retryable,
		"attempt_count", job.AttemptCount,
		"error", pe.Err)
	return domain.Wrap(pe.Err, pe.Kind, pe.Code, pe.Message)
}

// failQuietly records the terminal state. A failure to record it must not
// replace the original error, which is what the client needs to see.
func (s *Service) failQuietly(ctx context.Context, job domain.ProcessingJob, failure Failure) {
	// The client's context may already be done, which is exactly when the
	// record matters most, so the write gets its own short deadline.
	writeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), s.limits.FailureRecordTimeout)
	defer cancel()
	if _, err := s.store.FailJob(writeCtx, job.ID, s.now(), failure); err != nil {
		s.logger.ErrorContext(ctx, "job failure not recorded",
			"job_id", job.ID, "code", failure.Code, "error", err)
	}
}

// request builds the dispatch. Job identity, parameters and the request
// time come from the stored job, so what the processor sees is what was
// recorded.
func (s *Service) request(job domain.ProcessingJob, valid NewJob, readings []domain.Measurement) Request {
	types := make([]string, len(valid.Parameters.MeasurementTypes))
	for i, t := range valid.Parameters.MeasurementTypes {
		types[i] = string(t)
	}
	items := make([]Reading, len(readings))
	for i, m := range readings {
		items[i] = Reading{
			ID:         m.ID.String(),
			Type:       string(m.Type),
			Value:      m.Value,
			Unit:       m.Unit,
			RecordedAt: m.RecordedAt.UTC().Format(time.RFC3339Nano),
		}
	}
	return Request{
		Job: RequestJob{
			ID:        job.ID.String(),
			PatientID: job.PatientID.String(),
			Parameters: RequestParams{
				MeasurementTypes: types,
				Windows:          valid.Parameters.Windows,
				Percentiles:      percentilesOrEmpty(valid.Parameters.Percentiles),
			},
			AlgorithmVersion: job.AlgorithmVersion,
			// The processor validates timestamps against this instant, so
			// the outcome does not depend on when the job actually runs.
			Status:      domain.JobPending,
			RequestedAt: job.RequestedAt.UTC().Format(time.RFC3339Nano),
		},
		Readings: items,
	}
}

func percentilesOrEmpty(in []int) []int {
	if in == nil {
		return []int{}
	}
	return in
}

// storedResults converts the outcome into rows, rejecting anything that
// does not belong to this job or that the schema would refuse. A processor
// that answers with something unusable is a protocol failure, not a
// corrupted database.
func (s *Service) storedResults(outcome Outcome, sent int) ([]StoredResult, error) {
	protocol := func(reason string) error {
		return &ProcessorError{
			Code:    CodeProcessorProtocol,
			Message: "The processing service returned something this gateway cannot use.",
			Kind:    domain.KindUnavailable,
			Err:     errors.New(reason),
		}
	}
	if outcome.ServiceVersion == "" {
		return nil, protocol("the outcome names no service version")
	}
	// Every reading sent came from this gateway's own database, was
	// validated against the same catalogue, and is of a type the job asked
	// for, so the processor should accept all of them. If it did not, its
	// statistics cover less data than the job asked for, and storing them
	// would present a partial answer as a whole one.
	if outcome.Accepted != sent || outcome.Skipped != 0 || len(outcome.Rejected) != 0 {
		return nil, &ProcessorError{
			Code: CodeProcessingIncomplete,
			Message: "The processing service could not process every measurement in the range, " +
				"so no result is reported for it.",
			Kind: domain.KindUnavailable,
			Err: fmt.Errorf("sent %d readings; the processor accepted %d, skipped %d and rejected %d",
				sent, outcome.Accepted, outcome.Skipped, len(outcome.Rejected)),
		}
	}
	if outcome.AlgorithmVersion != s.limits.AlgorithmVersion {
		return nil, protocol(fmt.Sprintf("the outcome carries algorithm version %q, not %q",
			outcome.AlgorithmVersion, s.limits.AlgorithmVersion))
	}
	out := make([]StoredResult, 0, len(outcome.Results))
	for i, r := range outcome.Results {
		switch {
		case r.JobID != outcome.JobID:
			return nil, protocol(fmt.Sprintf("result %d belongs to job %q, not %q", i, r.JobID, outcome.JobID))
		case !slices.Contains(Windows, r.Window):
			return nil, protocol(fmt.Sprintf("result %d has window %q", i, r.Window))
		case r.MeasurementType == "":
			return nil, protocol(fmt.Sprintf("result %d names no measurement type", i))
		// A type outside the catalogue would be refused by the schema. It
		// is caught here so the answer is a failure of the processing
		// service, which it is, rather than a validation error blamed on
		// the client, which it is not.
		case !knownType(r.MeasurementType):
			return nil, protocol(fmt.Sprintf("result %d has measurement type %q, which is not in the catalogue", i, r.MeasurementType))
		case len(r.Statistics) == 0 || !json.Valid(r.Statistics):
			return nil, protocol(fmt.Sprintf("result %d has no usable statistics", i))
		case len(r.Anomalies) > 0 && !json.Valid(r.Anomalies):
			return nil, protocol(fmt.Sprintf("result %d has unusable anomalies", i))
		case r.WindowStart.IsZero():
			return nil, protocol(fmt.Sprintf("result %d has no window start", i))
		}
		anomalies := r.Anomalies
		if len(anomalies) == 0 {
			anomalies = json.RawMessage(`[]`)
		}
		out = append(out, StoredResult{
			MeasurementType:  domain.MeasurementType(r.MeasurementType),
			Window:           r.Window,
			WindowStart:      r.WindowStart.UTC(),
			Statistics:       r.Statistics,
			Anomalies:        anomalies,
			AlgorithmVersion: r.AlgorithmVersion,
			ServiceVersion:   r.ServiceVersion,
		})
	}
	return out, nil
}

// knownType reports whether the catalogue holds this measurement type.
func knownType(code string) bool {
	_, ok := measurement.Lookup(domain.MeasurementType(code))
	return ok
}

// Get returns one job. Jobs of a deleted patient follow the patient's
// visibility: ADMIN sees them, every other role does not.
func (s *Service) Get(ctx context.Context, actor auth.Principal, id uuid.UUID) (job domain.ProcessingJob, err error) {
	ctx, finish := s.begin(ctx, "processing.get")
	defer func() { finish(err) }()

	job, err = s.store.GetJob(ctx, id)
	if err != nil {
		return domain.ProcessingJob{}, translateJob(err)
	}
	if err := s.patientVisible(ctx, actor, job.PatientID); err != nil {
		// A job whose patient the caller cannot see is a job that does not
		// exist, so the response never confirms that the id is real.
		return domain.ProcessingJob{}, ErrJobNotFound()
	}
	return job, nil
}

// Results returns one page of a patient's results.
func (s *Service) Results(ctx context.Context, actor auth.Principal, patientID uuid.UUID, after *ResultCursor, limit int) (page Page, err error) {
	ctx, finish := s.begin(ctx, "processing.results")
	defer func() { finish(err) }()

	if err := s.patientVisible(ctx, actor, patientID); err != nil {
		return Page{}, err
	}
	items, err := s.store.ListResultsByPatient(ctx, patientID, after, limit+1)
	if err != nil {
		return Page{}, s.storeError(ctx, err)
	}
	if len(items) > limit {
		items = items[:limit]
		last := items[len(items)-1]
		cursor, err := pagination.EncodeCursor(ResultCursor{
			MeasurementType: last.MeasurementType,
			Window:          last.Window,
			WindowStart:     last.WindowStart,
			JobID:           last.JobID,
		})
		if err != nil {
			return Page{}, err
		}
		return Page{Items: items, NextCursor: cursor}, nil
	}
	return Page{Items: items}, nil
}

// patientVisible reports a not-found error when the patient does not exist,
// or is deleted and the actor is not an ADMIN.
func (s *Service) patientVisible(ctx context.Context, actor auth.Principal, id uuid.UUID) error {
	status, err := s.store.PatientStatus(ctx, id)
	if err != nil {
		var domErr *domain.Error
		if errors.As(err, &domErr) && domErr.Kind == domain.KindNotFound {
			return ErrPatientNotFound()
		}
		return s.storeError(ctx, err)
	}
	if status == domain.PatientDeleted && actor.Role != domain.RoleAdmin {
		return ErrPatientNotFound()
	}
	return nil
}

// validate turns an Input into a NewJob, reporting every offending field at
// once.
func (s *Service) validate(in Input) (NewJob, error) {
	v := validate.Validator{Code: CodeValidationFailed, Message: validationMessage}

	var patientID uuid.UUID
	v.Required("patient_id", in.PatientID)
	if in.PatientID != "" {
		id, err := uuid.Parse(in.PatientID)
		v.Check(err == nil && id != uuid.Nil, "patient_id", "must be a UUID")
		patientID = id
	}

	types := make([]domain.MeasurementType, 0, len(in.MeasurementTypes))
	v.Check(len(in.MeasurementTypes) > 0, "measurement_types", "must contain at least one measurement type")
	seenType := map[string]bool{}
	for i, raw := range in.MeasurementTypes {
		field := fmt.Sprintf("measurement_types[%d]", i)
		_, known := measurement.Lookup(domain.MeasurementType(raw))
		switch {
		case !known:
			v.Add(field, "must be one of: "+strings.Join(measurement.TypeCodes(), ", "))
		case seenType[raw]:
			v.Add(field, "is listed more than once")
		default:
			seenType[raw] = true
			types = append(types, domain.MeasurementType(raw))
		}
	}

	windows := make([]string, 0, len(in.Windows))
	v.Check(len(in.Windows) > 0, "windows", "must contain at least one window")
	seenWindow := map[string]bool{}
	for i, raw := range in.Windows {
		field := fmt.Sprintf("windows[%d]", i)
		switch {
		case !slices.Contains(Windows, raw):
			v.Add(field, "must be one of 1m, 5m, 15m, 1h, 6h, 24h, 7d")
		case seenWindow[raw]:
			v.Add(field, "is listed more than once")
		default:
			seenWindow[raw] = true
			windows = append(windows, raw)
		}
	}

	percentiles := make([]int, 0, len(in.Percentiles))
	v.Check(len(in.Percentiles) <= MaxPercentiles, "percentiles",
		fmt.Sprintf("must contain at most %d ranks", MaxPercentiles))
	seenRank := map[int]bool{}
	for i, rank := range in.Percentiles {
		field := fmt.Sprintf("percentiles[%d]", i)
		switch {
		case rank < 1 || rank > 99:
			v.Add(field, "must be between 1 and 99")
		case seenRank[rank]:
			v.Add(field, "is listed more than once")
		default:
			seenRank[rank] = true
			percentiles = append(percentiles, rank)
		}
	}

	optionalTime := func(field, value string) *time.Time {
		if value == "" {
			return nil
		}
		t, err := time.Parse(time.RFC3339Nano, value)
		if err != nil {
			v.Add(field, "must be an RFC 3339 timestamp with a UTC offset")
			return nil
		}
		// PostgreSQL stores microseconds; normalising here means the range
		// that is stored with the job is the range that was applied.
		t = t.UTC().Truncate(time.Microsecond)
		return &t
	}
	from := optionalTime("from", in.From)
	to := optionalTime("to", in.To)
	if from != nil && to != nil && !to.After(*from) {
		v.Add("to", "must be later than from")
	}

	if err := v.Err(); err != nil {
		return NewJob{}, err
	}
	// Sorting makes two requests that differ only in order identical, which
	// is what the processor does with them anyway.
	slices.Sort(types)
	slices.SortFunc(windows, func(a, b string) int {
		return slices.Index(Windows, a) - slices.Index(Windows, b)
	})
	slices.Sort(percentiles)
	return NewJob{
		PatientID: patientID,
		Parameters: Parameters{
			MeasurementTypes: types,
			Windows:          windows,
			Percentiles:      percentiles,
			From:             from,
			To:               to,
		},
	}, nil
}

// begin starts a span and returns the function that ends it.
func (s *Service) begin(ctx context.Context, name string) (context.Context, func(error)) {
	start := s.now()
	ctx, span := s.tracer.Start(ctx, name)
	return ctx, func(err error) {
		span.RecordError(err)
		span.End()
		s.metrics.Operation(name, outcomeOf(err), s.now().Sub(start))
	}
}

// outcomeOf classifies an operation for the metrics recorder.
func outcomeOf(err error) metrics.Outcome {
	if err == nil {
		return metrics.OutcomeOK
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return metrics.OutcomeCancelled
	}
	var domErr *domain.Error
	if !errors.As(err, &domErr) {
		return metrics.OutcomeError
	}
	switch domErr.Kind {
	case domain.KindInvalid, domain.KindValidation, domain.KindTooLarge, domain.KindUnsupportedMedia:
		return metrics.OutcomeInvalid
	case domain.KindNotFound:
		return metrics.OutcomeNotFound
	case domain.KindConflict:
		return metrics.OutcomeConflict
	case domain.KindUnauthorized, domain.KindForbidden:
		return metrics.OutcomeDenied
	default:
		return metrics.OutcomeError
	}
}

// storeError hides an unexpected persistence failure behind a generic
// error, keeping the cause for the log.
func (s *Service) storeError(ctx context.Context, err error) error {
	var domErr *domain.Error
	if errors.As(err, &domErr) {
		return domErr
	}
	s.logger.ErrorContext(ctx, "processing store failed", "error", err)
	return domain.Wrap(err, domain.KindInternal, "INTERNAL_ERROR", "The request could not be completed.")
}

// translateJob maps a repository not-found onto the API's job error.
func translateJob(err error) error {
	var domErr *domain.Error
	if errors.As(err, &domErr) && domErr.Kind == domain.KindNotFound {
		return ErrJobNotFound()
	}
	return err
}
