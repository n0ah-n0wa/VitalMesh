package processing

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/n0ah-n0wa/VitalMesh/services/api-gateway/internal/auth"
	"github.com/n0ah-n0wa/VitalMesh/services/api-gateway/internal/config"
	"github.com/n0ah-n0wa/VitalMesh/services/api-gateway/internal/domain"
	"github.com/n0ah-n0wa/VitalMesh/services/api-gateway/internal/requestid"
)

// ---------------------------------------------------------------- doubles

// fakeStore records every call in order, so a test can assert not only what
// happened but when. It also refuses to be used while the processor is
// being called, which is how the transaction-boundary rule is enforced.
type fakeStore struct {
	t *testing.T

	calls []string
	// insideCall is set by fakeProcessor for the duration of a dispatch.
	insideCall *bool

	job       domain.ProcessingJob
	readings  []domain.Measurement
	results   []domain.ProcessingResult
	status    domain.PatientStatus
	statusErr error

	createErr   error
	startErr    error
	completeErr error
	failErr     error
	getErr      error
	readingsErr error

	created   NewJob
	audit     AuditEvent
	completed []StoredResult
	failure   Failure
	failed    bool
}

func (f *fakeStore) record(name string) {
	if f.insideCall != nil && *f.insideCall {
		f.t.Helper()
		f.t.Fatalf("%s was called while the processor call was in flight: "+
			"no database work may overlap an external call", name)
	}
	f.calls = append(f.calls, name)
}

func (f *fakeStore) CreateJob(_ context.Context, in NewJob, event AuditEvent) (domain.ProcessingJob, error) {
	f.record("CreateJob")
	f.created, f.audit = in, event
	if f.createErr != nil {
		return domain.ProcessingJob{}, f.createErr
	}
	f.job.Status = domain.JobPending
	return f.job, nil
}

func (f *fakeStore) StartJob(_ context.Context, _ uuid.UUID, now time.Time, version string) (domain.ProcessingJob, error) {
	f.record("StartJob")
	if f.startErr != nil {
		return domain.ProcessingJob{}, f.startErr
	}
	started := f.job
	started.Status = domain.JobProcessing
	started.StartedAt = &now
	started.AttemptCount = 1
	started.ServiceVersion = &version
	f.job = started
	return started, nil
}

func (f *fakeStore) CompleteJob(_ context.Context, _ uuid.UUID, now time.Time, results []StoredResult, version string) (domain.ProcessingJob, error) {
	f.record("CompleteJob")
	f.completed = results
	if f.completeErr != nil {
		return domain.ProcessingJob{}, f.completeErr
	}
	done := f.job
	done.Status = domain.JobCompleted
	done.CompletedAt = &now
	done.ServiceVersion = &version
	return done, nil
}

func (f *fakeStore) FailJob(_ context.Context, _ uuid.UUID, now time.Time, failure Failure) (domain.ProcessingJob, error) {
	f.record("FailJob")
	f.failure, f.failed = failure, true
	if f.failErr != nil {
		return domain.ProcessingJob{}, f.failErr
	}
	failed := f.job
	failed.Status = domain.JobFailed
	failed.FailedAt = &now
	failed.ErrorCode, failed.ErrorMessage = &failure.Code, &failure.Message
	return failed, nil
}

func (f *fakeStore) GetJob(context.Context, uuid.UUID) (domain.ProcessingJob, error) {
	f.record("GetJob")
	if f.getErr != nil {
		return domain.ProcessingJob{}, f.getErr
	}
	return f.job, nil
}

func (f *fakeStore) ReadingsForJob(_ context.Context, _ uuid.UUID, _ Parameters, limit int) ([]domain.Measurement, error) {
	f.record("ReadingsForJob")
	if f.readingsErr != nil {
		return nil, f.readingsErr
	}
	if len(f.readings) > limit {
		return f.readings[:limit], nil
	}
	return f.readings, nil
}

func (f *fakeStore) ListResultsByPatient(_ context.Context, _ uuid.UUID, _ *ResultCursor, limit int) ([]domain.ProcessingResult, error) {
	f.record("ListResultsByPatient")
	if len(f.results) > limit {
		return f.results[:limit], nil
	}
	return f.results, nil
}

func (f *fakeStore) PatientStatus(context.Context, uuid.UUID) (domain.PatientStatus, error) {
	f.record("PatientStatus")
	if f.statusErr != nil {
		return "", f.statusErr
	}
	return f.status, nil
}

// fakeProcessor stands in for the Rust service. While Process runs it
// raises inFlight, so the store can prove that no database work overlaps an
// external call.
type fakeProcessor struct {
	outcome Outcome
	err     error
	// keepAccounting leaves the outcome's counts alone, for tests about
	// what happens when they do not add up.
	keepAccounting bool
	inFlight       *bool
	got            Request
	calls          int
}

func (p *fakeProcessor) Process(_ context.Context, req Request) (Outcome, error) {
	p.calls++
	p.got = req
	if p.inFlight != nil {
		*p.inFlight = true
		defer func() { *p.inFlight = false }()
	}
	if p.err != nil {
		return Outcome{}, p.err
	}
	out := p.outcome
	if out.JobID == "" {
		out.JobID = req.Job.ID
	}
	if !p.keepAccounting {
		// A real processor accepts every reading a healthy gateway sends.
		out.Accepted = len(req.Readings)
	}
	for i := range out.Results {
		if out.Results[i].JobID == "" {
			out.Results[i].JobID = out.JobID
		}
	}
	return out, nil
}

// ---------------------------------------------------------------- helpers

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func limits() config.Processing {
	return config.Processing{
		MaxJobMeasurements:   1000,
		AlgorithmVersion:     "1.0.0",
		ServiceVersion:       "api-gateway-test",
		FailureRecordTimeout: time.Second,
	}
}

func actor() auth.Principal {
	return auth.Principal{UserID: uuid.New(), Role: domain.RoleOperator}
}

func admin() auth.Principal {
	return auth.Principal{UserID: uuid.New(), Role: domain.RoleAdmin}
}

func testContext() context.Context {
	ctx := requestid.NewContext(context.Background(), "req-1")
	return requestid.NewCorrelationContext(ctx, "corr-1")
}

func readings(n int) []domain.Measurement {
	base := time.Date(2026, 9, 6, 11, 0, 0, 0, time.UTC)
	out := make([]domain.Measurement, n)
	for i := range out {
		out[i] = domain.Measurement{
			ID:         uuid.New(),
			Type:       "HEART_RATE",
			Value:      60 + float64(i%7),
			Unit:       "bpm",
			RecordedAt: base.Add(time.Duration(i) * time.Second),
		}
	}
	return out
}

func newService(t *testing.T, store *fakeStore, processor Processor) *Service {
	t.Helper()
	store.t = t
	if store.job.ID == uuid.Nil {
		store.job = domain.ProcessingJob{
			ID:               uuid.New(),
			PatientID:        uuid.New(),
			AlgorithmVersion: "1.0.0",
			RequestedAt:      time.Date(2026, 9, 6, 12, 0, 0, 0, time.UTC),
		}
	}
	if store.status == "" {
		store.status = domain.PatientActive
	}
	return NewService(store, processor, limits(), discardLogger(), Options{})
}

func validInput(patientID uuid.UUID) Input {
	return Input{
		PatientID:        patientID.String(),
		MeasurementTypes: []string{"HEART_RATE"},
		Windows:          []string{"1h", "1m"},
		Percentiles:      []int{95, 50},
	}
}

func domainError(t *testing.T, err error) *domain.Error {
	t.Helper()
	var domErr *domain.Error
	if !errors.As(err, &domErr) {
		t.Fatalf("error %v is not a domain error", err)
	}
	return domErr
}

// okOutcome is a well-formed outcome. Accepted is set by the fake processor
// to the number of readings it was sent, so the accounting adds up.
func okOutcome() Outcome {
	return Outcome{
		AlgorithmVersion: "1.0.0",
		ServiceVersion:   "processor-1",
		Results: []Result{{
			MeasurementType:  "HEART_RATE",
			Window:           "1h",
			WindowStart:      time.Date(2026, 9, 6, 11, 0, 0, 0, time.UTC),
			Statistics:       json.RawMessage(`{"count":3}`),
			Anomalies:        json.RawMessage(`[]`),
			AlgorithmVersion: "1.0.0",
			ServiceVersion:   "processor-1",
		}},
	}
}

// ------------------------------------------------------------ happy path

func TestCreateRunsTheJobThroughItsLifecycleInOrder(t *testing.T) {
	store := &fakeStore{readings: readings(3)}
	// While the processor is being called, any database work would mean a
	// transaction held open across an external call.
	inFlight := false
	processor := &fakeProcessor{outcome: okOutcome(), inFlight: &inFlight}
	store.insideCall = &inFlight
	svc := newService(t, store, processor)

	job, err := svc.Create(testContext(), actor(), validInput(store.job.PatientID))
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	if job.Status != domain.JobCompleted {
		t.Errorf("status = %s, want COMPLETED", job.Status)
	}
	want := []string{"PatientStatus", "CreateJob", "ReadingsForJob", "StartJob", "CompleteJob"}
	if strings.Join(store.calls, ",") != strings.Join(want, ",") {
		t.Errorf("call order = %v, want %v", store.calls, want)
	}
	if store.failed {
		t.Error("a successful job must not be recorded as failed")
	}
	if len(store.completed) != 1 {
		t.Fatalf("stored %d results, want 1", len(store.completed))
	}
	if got := store.completed[0].MeasurementType; got != "HEART_RATE" {
		t.Errorf("stored type = %s", got)
	}
}

func TestCreateRecordsAnAuditEventForTheJob(t *testing.T) {
	store := &fakeStore{readings: readings(1)}
	svc := newService(t, store, &fakeProcessor{outcome: okOutcome()})
	who := actor()

	if _, err := svc.Create(testContext(), who, validInput(store.job.PatientID)); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if store.audit.Action != domain.AuditProcessingJobCreated {
		t.Errorf("audit action = %s, want PROCESSING_JOB_CREATED", store.audit.Action)
	}
	if store.audit.ActorID != who.UserID {
		t.Error("the audit record must name the actor")
	}
	if store.audit.RequestID != "req-1" {
		t.Errorf("audit request id = %q, want the request's own", store.audit.RequestID)
	}
}

func TestCreateSendsTheProcessorWhatWasRecorded(t *testing.T) {
	store := &fakeStore{readings: readings(2)}
	processor := &fakeProcessor{outcome: okOutcome()}
	svc := newService(t, store, processor)

	if _, err := svc.Create(testContext(), actor(), validInput(store.job.PatientID)); err != nil {
		t.Fatalf("Create: %v", err)
	}

	got := processor.got
	if got.Job.ID != store.job.ID.String() {
		t.Errorf("dispatched job id = %s, want the stored one", got.Job.ID)
	}
	if got.Job.AlgorithmVersion != "1.0.0" {
		t.Errorf("algorithm version = %s", got.Job.AlgorithmVersion)
	}
	if got.Job.Status != domain.JobPending {
		t.Errorf("the processor is told the job is %s; the contract's job carries PENDING", got.Job.Status)
	}
	if got.Job.RequestedAt != "2026-09-06T12:00:00Z" {
		t.Errorf("requested_at = %q, want the stored job's own instant", got.Job.RequestedAt)
	}
	// Parameters are normalised: sorted, de-duplicated, in window order.
	if strings.Join(got.Job.Parameters.Windows, ",") != "1m,1h" {
		t.Errorf("windows = %v, want them in window order", got.Job.Parameters.Windows)
	}
	if got.Job.Parameters.Percentiles[0] != 50 {
		t.Errorf("percentiles = %v, want them sorted", got.Job.Parameters.Percentiles)
	}
	if len(got.Readings) != 2 {
		t.Fatalf("sent %d readings, want 2", len(got.Readings))
	}
	if got.Readings[0].Unit != "bpm" || got.Readings[0].Type != "HEART_RATE" {
		t.Errorf("reading not mapped: %+v", got.Readings[0])
	}
	if !strings.HasSuffix(got.Readings[0].RecordedAt, "Z") {
		t.Errorf("recorded_at = %q, want UTC", got.Readings[0].RecordedAt)
	}
}

// ------------------------------------------------------------ validation

func TestCreateRejectsInvalidInput(t *testing.T) {
	patientID := uuid.New()
	cases := []struct {
		name  string
		input Input
		field string
	}{
		{"no patient", Input{MeasurementTypes: []string{"HEART_RATE"}, Windows: []string{"1h"}}, "patient_id"},
		{"patient not a uuid", Input{PatientID: "nope", MeasurementTypes: []string{"HEART_RATE"}, Windows: []string{"1h"}}, "patient_id"},
		{"no types", Input{PatientID: patientID.String(), Windows: []string{"1h"}}, "measurement_types"},
		{"unknown type", Input{PatientID: patientID.String(), MeasurementTypes: []string{"PULSE"}, Windows: []string{"1h"}}, "measurement_types[0]"},
		{"repeated type", Input{PatientID: patientID.String(), MeasurementTypes: []string{"HEART_RATE", "HEART_RATE"}, Windows: []string{"1h"}}, "measurement_types[1]"},
		{"no windows", Input{PatientID: patientID.String(), MeasurementTypes: []string{"HEART_RATE"}}, "windows"},
		{"unknown window", Input{PatientID: patientID.String(), MeasurementTypes: []string{"HEART_RATE"}, Windows: []string{"3h"}}, "windows[0]"},
		{"repeated window", Input{PatientID: patientID.String(), MeasurementTypes: []string{"HEART_RATE"}, Windows: []string{"1h", "1h"}}, "windows[1]"},
		{"percentile out of range", Input{PatientID: patientID.String(), MeasurementTypes: []string{"HEART_RATE"}, Windows: []string{"1h"}, Percentiles: []int{100}}, "percentiles[0]"},
		{"repeated percentile", Input{PatientID: patientID.String(), MeasurementTypes: []string{"HEART_RATE"}, Windows: []string{"1h"}, Percentiles: []int{50, 50}}, "percentiles[1]"},
		{"unparseable from", Input{PatientID: patientID.String(), MeasurementTypes: []string{"HEART_RATE"}, Windows: []string{"1h"}, From: "yesterday"}, "from"},
		{"to before from", Input{PatientID: patientID.String(), MeasurementTypes: []string{"HEART_RATE"}, Windows: []string{"1h"},
			From: "2026-09-06T12:00:00Z", To: "2026-09-06T11:00:00Z"}, "to"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := &fakeStore{}
			svc := newService(t, store, &fakeProcessor{})

			_, err := svc.Create(testContext(), actor(), tc.input)
			domErr := domainError(t, err)
			if domErr.Kind != domain.KindValidation {
				t.Errorf("kind = %s, want validation", domErr.Kind)
			}
			if domErr.Code != CodeValidationFailed {
				t.Errorf("code = %s", domErr.Code)
			}
			found := false
			for _, d := range domErr.Details {
				if d.Field == tc.field {
					found = true
				}
			}
			if !found {
				t.Errorf("details %+v do not name %q", domErr.Details, tc.field)
			}
			if len(store.calls) != 0 {
				t.Errorf("an invalid request touched the database: %v", store.calls)
			}
		})
	}
}

func TestCreateReportsEveryInvalidFieldAtOnce(t *testing.T) {
	svc := newService(t, &fakeStore{}, &fakeProcessor{})
	_, err := svc.Create(testContext(), actor(), Input{
		PatientID:        "not-a-uuid",
		MeasurementTypes: []string{"PULSE"},
		Windows:          []string{"3h"},
	})
	domErr := domainError(t, err)
	if len(domErr.Details) < 3 {
		t.Errorf("details = %+v, want every problem reported at once", domErr.Details)
	}
}

// -------------------------------------------------- patient visibility

func TestCreateRefusesAnUnknownOrInvisiblePatient(t *testing.T) {
	t.Run("unknown", func(t *testing.T) {
		store := &fakeStore{statusErr: domain.New(domain.KindNotFound, "PATIENT_NOT_FOUND", "no")}
		svc := newService(t, store, &fakeProcessor{})
		_, err := svc.Create(testContext(), actor(), validInput(uuid.New()))
		if domainError(t, err).Code != CodePatientNotFound {
			t.Errorf("code = %s", domainError(t, err).Code)
		}
	})

	t.Run("deleted patient is invisible to an operator", func(t *testing.T) {
		store := &fakeStore{status: domain.PatientDeleted}
		svc := newService(t, store, &fakeProcessor{})
		_, err := svc.Create(testContext(), actor(), validInput(uuid.New()))
		if domainError(t, err).Kind != domain.KindNotFound {
			t.Error("a deleted patient must look absent to an operator")
		}
	})

	t.Run("deleted patient is visible to an admin", func(t *testing.T) {
		store := &fakeStore{status: domain.PatientDeleted, readings: readings(1)}
		svc := newService(t, store, &fakeProcessor{outcome: okOutcome()})
		if _, err := svc.Create(testContext(), admin(), validInput(store.job.PatientID)); err != nil {
			t.Fatalf("an admin may process a deleted patient's data: %v", err)
		}
	})
}

// ----------------------------------------------------- nothing to process

func TestCreateFailsTheJobWhenThereIsNothingToProcess(t *testing.T) {
	store := &fakeStore{readings: nil}
	svc := newService(t, store, &fakeProcessor{})

	_, err := svc.Create(testContext(), actor(), validInput(store.job.PatientID))
	domErr := domainError(t, err)
	if domErr.Code != CodeNoMeasurements {
		t.Errorf("code = %s, want %s", domErr.Code, CodeNoMeasurements)
	}
	if domErr.Kind != domain.KindValidation {
		t.Errorf("kind = %s", domErr.Kind)
	}
	// The job was created, so it must not be left dangling in PENDING.
	if !store.failed {
		t.Fatal("the job must be recorded as failed, not abandoned in PENDING")
	}
	if store.failure.Code != CodeNoMeasurements {
		t.Errorf("recorded code = %s", store.failure.Code)
	}
	want := []string{"PatientStatus", "CreateJob", "ReadingsForJob", "StartJob", "FailJob"}
	if strings.Join(store.calls, ",") != strings.Join(want, ",") {
		t.Errorf("call order = %v, want %v", store.calls, want)
	}
}

func TestCreateRefusesAJobLargerThanTheBound(t *testing.T) {
	store := &fakeStore{readings: readings(1001)}
	svc := newService(t, store, &fakeProcessor{})

	_, err := svc.Create(testContext(), actor(), validInput(store.job.PatientID))
	domErr := domainError(t, err)
	if domErr.Code != CodeJobTooLarge {
		t.Errorf("code = %s, want %s", domErr.Code, CodeJobTooLarge)
	}
	if !strings.Contains(domErr.Message, "narrow it") {
		t.Errorf("message %q should tell the client what to do", domErr.Message)
	}
	if !store.failed {
		t.Error("the job must be recorded as failed")
	}
}

// ------------------------------------------------- processor failures

func TestCreateRecordsAndMapsAProcessorFailure(t *testing.T) {
	cases := []struct {
		name      string
		err       *ProcessorError
		wantCode  string
		wantKind  domain.Kind
		retryable bool
	}{
		{
			name: "unavailable",
			err: &ProcessorError{Code: CodeProcessorUnavailable, Message: "The processing service is unavailable; retry later.",
				Kind: domain.KindUnavailable, Retryable: true},
			wantCode: CodeProcessorUnavailable, wantKind: domain.KindUnavailable, retryable: true,
		},
		{
			name: "timeout",
			err: &ProcessorError{Code: CodeProcessorTimeout, Message: "The processing service did not answer in time.",
				Kind: domain.KindTimeout, Retryable: true},
			wantCode: CodeProcessorTimeout, wantKind: domain.KindTimeout, retryable: true,
		},
		{
			name: "rejected",
			err: &ProcessorError{Code: CodeProcessorRejected, Message: "The processing service could not process the job.",
				Kind: domain.KindValidation},
			wantCode: CodeProcessorRejected, wantKind: domain.KindValidation,
		},
		{
			name: "protocol",
			err: &ProcessorError{Code: CodeProcessorProtocol, Message: "The processing service could not be used.",
				Kind: domain.KindUnavailable},
			wantCode: CodeProcessorProtocol, wantKind: domain.KindUnavailable,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := &fakeStore{readings: readings(2)}
			svc := newService(t, store, &fakeProcessor{err: tc.err})

			_, err := svc.Create(testContext(), actor(), validInput(store.job.PatientID))
			domErr := domainError(t, err)
			if domErr.Code != tc.wantCode {
				t.Errorf("code = %s, want %s", domErr.Code, tc.wantCode)
			}
			if domErr.Kind != tc.wantKind {
				t.Errorf("kind = %s, want %s", domErr.Kind, tc.wantKind)
			}
			if !store.failed {
				t.Fatal("a failed dispatch must leave the job FAILED, never absent")
			}
			if store.failure.Code != tc.wantCode {
				t.Errorf("recorded code = %s, want %s", store.failure.Code, tc.wantCode)
			}
			// The job reached PROCESSING before the call, so the attempt is
			// counted even though it failed.
			want := []string{"PatientStatus", "CreateJob", "ReadingsForJob", "StartJob", "FailJob"}
			if strings.Join(store.calls, ",") != strings.Join(want, ",") {
				t.Errorf("call order = %v, want %v", store.calls, want)
			}
		})
	}
}

func TestAFailureNeverCarriesTheProcessorsOwnWords(t *testing.T) {
	store := &fakeStore{readings: readings(1)}
	svc := newService(t, store, &fakeProcessor{err: &ProcessorError{
		Code:    CodeProcessorUnavailable,
		Message: "The processing service is unavailable; retry later.",
		Kind:    domain.KindUnavailable,
		Err:     errors.New("dial tcp 10.1.2.3:8081: connection refused"),
	}})

	_, err := svc.Create(testContext(), actor(), validInput(store.job.PatientID))
	domErr := domainError(t, err)
	if strings.Contains(domErr.Message, "10.1.2.3") || strings.Contains(domErr.Message, "dial") {
		t.Errorf("the client-facing message leaked internal detail: %q", domErr.Message)
	}
	if strings.Contains(store.failure.Message, "10.1.2.3") {
		t.Errorf("the stored message leaked internal detail: %q", store.failure.Message)
	}
	if domErr.Err == nil {
		t.Error("the cause must be kept for the log")
	}
}

func TestAnUnclassifiedFailureIsTreatedAsAProtocolFailure(t *testing.T) {
	store := &fakeStore{readings: readings(1)}
	svc := newService(t, store, &fakeProcessor{err: errors.New("something nobody classified")})

	_, err := svc.Create(testContext(), actor(), validInput(store.job.PatientID))
	if domainError(t, err).Code != CodeProcessorProtocol {
		t.Errorf("code = %s, want %s", domainError(t, err).Code, CodeProcessorProtocol)
	}
	if !store.failed {
		t.Error("the job must still be recorded as failed")
	}
}

// -------------------------------------------- malformed processor output

func TestAnUnusableOutcomeIsAProtocolFailureAndStoresNothing(t *testing.T) {
	base := okOutcome()
	cases := []struct {
		name    string
		outcome func(Outcome) Outcome
	}{
		{"no service version", func(o Outcome) Outcome { o.ServiceVersion = ""; return o }},
		{"foreign algorithm version", func(o Outcome) Outcome { o.AlgorithmVersion = "2.0.0"; return o }},
		{"result of another job", func(o Outcome) Outcome { o.Results[0].JobID = uuid.New().String(); return o }},
		{"unknown window", func(o Outcome) Outcome { o.Results[0].Window = "3h"; return o }},
		{"no measurement type", func(o Outcome) Outcome { o.Results[0].MeasurementType = ""; return o }},
		{"no statistics", func(o Outcome) Outcome { o.Results[0].Statistics = nil; return o }},
		{"unusable statistics", func(o Outcome) Outcome { o.Results[0].Statistics = json.RawMessage(`{`); return o }},
		{"unusable anomalies", func(o Outcome) Outcome { o.Results[0].Anomalies = json.RawMessage(`[`); return o }},
		{"no window start", func(o Outcome) Outcome { o.Results[0].WindowStart = time.Time{}; return o }},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			outcome := base
			outcome.Results = append([]Result(nil), base.Results...)
			store := &fakeStore{readings: readings(1)}
			jobID := store.job.ID.String()
			outcome.JobID = jobID
			outcome.Results[0].JobID = jobID
			svc := newService(t, store, &fakeProcessor{outcome: tc.outcome(outcome)})

			_, err := svc.Create(testContext(), actor(), validInput(store.job.PatientID))
			domErr := domainError(t, err)
			if domErr.Code != CodeProcessorProtocol {
				t.Fatalf("code = %s, want %s", domErr.Code, CodeProcessorProtocol)
			}
			if store.completed != nil {
				t.Error("nothing may be stored from an outcome the gateway cannot trust")
			}
			if !store.failed {
				t.Error("the job must be recorded as failed")
			}
		})
	}
}

// An outcome whose counts do not add up means the processor did not process
// everything it was sent, so the statistics cover less data than the job
// asked for.
func TestAnIncompleteOutcomeFailsTheJobAndStoresNothing(t *testing.T) {
	cases := []struct {
		name    string
		outcome func(Outcome) Outcome
	}{
		{"readings were rejected", func(o Outcome) Outcome {
			o.Accepted, o.Rejected = 1, []any{"one", "two"}
			return o
		}},
		{"readings were skipped", func(o Outcome) Outcome {
			o.Accepted, o.Skipped = 2, 1
			return o
		}},
		{"fewer accepted than sent, with nothing to explain it", func(o Outcome) Outcome {
			o.Accepted = 1
			return o
		}},
		{"more accepted than sent", func(o Outcome) Outcome {
			o.Accepted = 99
			return o
		}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := &fakeStore{readings: readings(3)}
			outcome := okOutcome()
			outcome.Results = append([]Result(nil), outcome.Results...)
			svc := newService(t, store, &fakeProcessor{
				outcome:        tc.outcome(outcome),
				keepAccounting: true,
			})

			_, err := svc.Create(testContext(), actor(), validInput(store.job.PatientID))
			domErr := domainError(t, err)
			if domErr.Code != CodeProcessingIncomplete {
				t.Fatalf("code = %s, want %s", domErr.Code, CodeProcessingIncomplete)
			}
			if store.completed != nil {
				t.Error("no result may be stored from a partial run")
			}
			if !store.failed {
				t.Error("the job must be recorded as failed")
			}
			if store.failure.Code != CodeProcessingIncomplete {
				t.Errorf("recorded code = %s", store.failure.Code)
			}
			// The counts belong in the log, not in the client's message.
			if strings.Contains(domErr.Message, "99") || strings.Contains(domErr.Message, "sent") {
				t.Errorf("the message exposes internal accounting: %q", domErr.Message)
			}
		})
	}
}

// A job whose readings were all processed completes, even when a window
// happened to produce nothing.
func TestAnOutcomeWithNoResultsStillCompletesTheJob(t *testing.T) {
	store := &fakeStore{readings: readings(1)}
	outcome := okOutcome()
	outcome.Results = nil
	svc := newService(t, store, &fakeProcessor{outcome: outcome})

	job, err := svc.Create(testContext(), actor(), validInput(store.job.PatientID))
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if job.Status != domain.JobCompleted {
		t.Errorf("status = %s, want COMPLETED: a window with no data is not a failure", job.Status)
	}
	if len(store.completed) != 0 {
		t.Errorf("stored %d results, want none", len(store.completed))
	}
}

// ------------------------------------------------------------------ Get

func TestGetReturnsTheJobAndHidesWhatTheCallerMayNotSee(t *testing.T) {
	t.Run("found", func(t *testing.T) {
		store := &fakeStore{}
		svc := newService(t, store, &fakeProcessor{})
		job, err := svc.Get(testContext(), actor(), store.job.ID)
		if err != nil {
			t.Fatalf("Get: %v", err)
		}
		if job.ID != store.job.ID {
			t.Error("the wrong job came back")
		}
	})

	t.Run("unknown", func(t *testing.T) {
		store := &fakeStore{getErr: domain.New(domain.KindNotFound, "NOT_FOUND", "no")}
		svc := newService(t, store, &fakeProcessor{})
		_, err := svc.Get(testContext(), actor(), uuid.New())
		if domainError(t, err).Code != CodeJobNotFound {
			t.Errorf("code = %s", domainError(t, err).Code)
		}
	})

	t.Run("job of a deleted patient is absent for an operator", func(t *testing.T) {
		store := &fakeStore{status: domain.PatientDeleted}
		svc := newService(t, store, &fakeProcessor{})
		_, err := svc.Get(testContext(), actor(), store.job.ID)
		domErr := domainError(t, err)
		if domErr.Code != CodeJobNotFound {
			t.Errorf("code = %s, want the job to look absent", domErr.Code)
		}
	})

	t.Run("job of a deleted patient is visible to an admin", func(t *testing.T) {
		store := &fakeStore{status: domain.PatientDeleted}
		svc := newService(t, store, &fakeProcessor{})
		if _, err := svc.Get(testContext(), admin(), store.job.ID); err != nil {
			t.Fatalf("an admin may read it: %v", err)
		}
	})
}

// -------------------------------------------------------------- Results

func TestResultsPagesAndRefusesAnInvisiblePatient(t *testing.T) {
	patientID := uuid.New()
	items := make([]domain.ProcessingResult, 3)
	for i := range items {
		items[i] = domain.ProcessingResult{
			ID: uuid.New(), JobID: uuid.New(), PatientID: patientID,
			MeasurementType: "HEART_RATE", Window: "1h",
			WindowStart: time.Date(2026, 9, 6, i, 0, 0, 0, time.UTC),
		}
	}

	t.Run("a full page carries a cursor", func(t *testing.T) {
		store := &fakeStore{results: items}
		svc := newService(t, store, &fakeProcessor{})
		page, err := svc.Results(testContext(), actor(), patientID, nil, 2)
		if err != nil {
			t.Fatalf("Results: %v", err)
		}
		if len(page.Items) != 2 {
			t.Fatalf("returned %d items, want the page size", len(page.Items))
		}
		if page.NextCursor == "" {
			t.Error("a full page must offer a cursor")
		}
	})

	t.Run("the last page has no cursor", func(t *testing.T) {
		store := &fakeStore{results: items[:1]}
		svc := newService(t, store, &fakeProcessor{})
		page, err := svc.Results(testContext(), actor(), patientID, nil, 10)
		if err != nil {
			t.Fatalf("Results: %v", err)
		}
		if page.NextCursor != "" {
			t.Error("the last page must not offer a cursor")
		}
	})

	t.Run("an invisible patient has no results", func(t *testing.T) {
		store := &fakeStore{status: domain.PatientDeleted, results: items}
		svc := newService(t, store, &fakeProcessor{})
		_, err := svc.Results(testContext(), actor(), patientID, nil, 10)
		if domainError(t, err).Kind != domain.KindNotFound {
			t.Error("a deleted patient's results must not be listed for an operator")
		}
	})
}

// ------------------------------------------------------- store failures

func TestAStoreFailureIsHiddenFromTheClient(t *testing.T) {
	store := &fakeStore{createErr: errors.New("connection reset by peer")}
	svc := newService(t, store, &fakeProcessor{})

	_, err := svc.Create(testContext(), actor(), validInput(uuid.New()))
	domErr := domainError(t, err)
	if domErr.Kind != domain.KindInternal {
		t.Errorf("kind = %s, want internal", domErr.Kind)
	}
	if strings.Contains(domErr.Message, "connection reset") {
		t.Errorf("the message leaked the cause: %q", domErr.Message)
	}
}

func TestAFailureToRecordTheFailureDoesNotReplaceTheOriginalError(t *testing.T) {
	store := &fakeStore{readings: readings(1), failErr: errors.New("database is gone")}
	svc := newService(t, store, &fakeProcessor{err: &ProcessorError{
		Code: CodeProcessorUnavailable, Message: "unavailable", Kind: domain.KindUnavailable, Retryable: true,
	}})

	_, err := svc.Create(testContext(), actor(), validInput(store.job.PatientID))
	if domainError(t, err).Code != CodeProcessorUnavailable {
		t.Errorf("code = %s, want the original failure to survive", domainError(t, err).Code)
	}
}

// The record of why a job failed matters most when the caller's deadline
// has already passed, so it must not be written on a context that is done.
func TestTheFailureRecordIsWrittenEvenWhenTheCallerHasGivenUp(t *testing.T) {
	store := &fakeStore{readings: readings(1)}
	svc := newService(t, store, &fakeProcessor{err: &ProcessorError{
		Code: CodeProcessorTimeout, Message: "timeout", Kind: domain.KindTimeout, Retryable: true,
	}})

	ctx, cancel := context.WithCancel(testContext())
	// The processor "fails" and the caller gives up at the same moment.
	cancel()

	_, err := svc.Create(ctx, actor(), validInput(store.job.PatientID))
	if err == nil {
		t.Fatal("Create should have failed")
	}
	if !store.failed {
		t.Error("the failure must still be recorded after the caller gave up")
	}
}
