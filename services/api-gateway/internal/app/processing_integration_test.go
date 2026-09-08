//go:build integration

package app

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/n0ah-n0wa/VitalMesh/services/api-gateway/internal/auth"
	"github.com/n0ah-n0wa/VitalMesh/services/api-gateway/internal/config"
	"github.com/n0ah-n0wa/VitalMesh/services/api-gateway/internal/domain"
	"github.com/n0ah-n0wa/VitalMesh/services/api-gateway/internal/httpapi/middleware"
	"github.com/n0ah-n0wa/VitalMesh/services/api-gateway/internal/httpapi/model"
	"github.com/n0ah-n0wa/VitalMesh/services/api-gateway/internal/idempotency"
	"github.com/n0ah-n0wa/VitalMesh/services/api-gateway/internal/infra/postgres"
	"github.com/n0ah-n0wa/VitalMesh/services/api-gateway/internal/infra/postgres/postgrestest"
	"github.com/n0ah-n0wa/VitalMesh/services/api-gateway/internal/observability/logging"
	"github.com/n0ah-n0wa/VitalMesh/services/api-gateway/internal/processing"
	"github.com/n0ah-n0wa/VitalMesh/services/api-gateway/internal/requestid"
)

// The Processing API against a real PostgreSQL and a stand-in processor
// that can be told how to behave. What this proves that the unit tests
// cannot: the job rows, the results, the audit record and the idempotency
// records are really written, the state machine the database enforces is
// respected, and a failure leaves exactly the row it should.

// processorStub answers the internal API the way the contract says, and can
// be switched between behaviours between requests.
type processorStub struct {
	server   *httptest.Server
	handler  atomic.Pointer[func(http.ResponseWriter, *http.Request)]
	requests atomic.Int32
	// lastBody is the last dispatch received, for asserting what was sent.
	lastBody atomic.Pointer[[]byte]
	lastHead atomic.Pointer[http.Header]
}

func newProcessorStub(t *testing.T) *processorStub {
	t.Helper()
	s := &processorStub{}
	s.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.requests.Add(1)
		body, _ := io.ReadAll(r.Body)
		s.lastBody.Store(&body)
		head := r.Header.Clone()
		s.lastHead.Store(&head)
		if h := s.handler.Load(); h != nil {
			(*h)(w, r)
			return
		}
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	t.Cleanup(s.server.Close)
	return s
}

func (s *processorStub) answer(f func(http.ResponseWriter, *http.Request)) {
	s.handler.Store(&f)
}

// succeed makes the stub return a well-formed outcome derived from the
// dispatch it received, as the real processor would.
func (s *processorStub) succeed() {
	s.answer(func(w http.ResponseWriter, r *http.Request) {
		var req processing.Request
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			// The body was already consumed by the recorder; re-read it.
			body := s.lastBody.Load()
			if body == nil || json.Unmarshal(*body, &req) != nil {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
		}
		windowStart := time.Date(2026, 9, 6, 11, 0, 0, 0, time.UTC)
		results := make([]processing.Result, 0, len(req.Job.Parameters.Windows))
		for _, window := range req.Job.Parameters.Windows {
			results = append(results, processing.Result{
				JobID:            req.Job.ID,
				MeasurementType:  "HEART_RATE",
				Window:           window,
				WindowStart:      windowStart,
				Statistics:       json.RawMessage(`{"count":3,"min":60,"max":72,"mean":66,"median":66,"variance":36,"std_dev":6}`),
				Anomalies:        json.RawMessage(`[]`),
				AlgorithmVersion: config.DefaultAlgorithmVersion,
				ServiceVersion:   "processor-stub",
			})
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(processing.Outcome{
			JobID:            req.Job.ID,
			AlgorithmVersion: config.DefaultAlgorithmVersion,
			ServiceVersion:   "processor-stub",
			Accepted:         len(req.Readings),
			Rejected:         []any{},
			Results:          results,
		})
	})
}

func (s *processorStub) fail(status int, code string) {
	s.answer(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = fmt.Fprintf(w, `{"error":{"code":%q,"message":"internal detail at pod-9","request_id":"r","retryable":false}}`, code)
	})
}

// processingHarness is a wired gateway plus the fixtures the tests share.
type processingHarness struct {
	app       *App
	stub      *processorStub
	pool      any
	token     string
	patientID uuid.UUID
	userID    uuid.UUID
	logs      *bytes.Buffer
	jobs      *postgres.Jobs
	results   *postgres.Results
	audit     *postgres.Audit
	ctx       context.Context
}

func newProcessingHarness(t *testing.T, readings int, tune map[string]string) *processingHarness {
	t.Helper()
	pool, dbURL, _ := postgrestest.New(t)
	stub := newProcessorStub(t)

	cfg, err := config.Load(func(key string) (string, bool) {
		if v, ok := tune[key]; ok {
			return v, true
		}
		switch key {
		case "DATABASE_URL":
			return dbURL, true
		case "JWT_SECRET":
			return "integration-secret-integration-secret", true
		case "PASSWORD_HASH_MEMORY_KIB":
			return "8192", true
		case "PASSWORD_HASH_TIME":
			return "1", true
		case "PROCESSOR_URL":
			return stub.server.URL, true
		case "PROCESSOR_TOKEN":
			return "integration-processor-token", true
		case "PROCESSOR_TIMEOUT":
			return "2s", true
		case "PROCESSOR_BACKOFF":
			return "1ms", true
		case "PROCESSOR_MAX_BACKOFF":
			return "2ms", true
		case "HTTP_REQUEST_TIMEOUT":
			return "20s", true
		case "HTTP_WRITE_TIMEOUT":
			return "25s", true
		}
		return "", false
	})
	if err != nil {
		t.Fatalf("config: %v", err)
	}

	logs := &bytes.Buffer{}
	a, err := New(context.Background(), cfg, logging.New(logs, config.Log{Format: config.LogFormatJSON}, logging.Service{Name: "test"}), "v-int")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(a.pool.Close)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	t.Cleanup(cancel)

	hash, err := auth.NewHasher(cfg.Auth.Password).Hash(ctx, "integration-password")
	if err != nil {
		t.Fatal(err)
	}
	operator, err := postgres.NewUsers(pool).Create(ctx, "processing@example.com", hash, domain.RoleOperator)
	if err != nil {
		t.Fatal(err)
	}
	patient, err := postgres.NewPatients(pool).Create(ctx, "proc-patient", time.Date(1980, 1, 1, 0, 0, 0, 0, time.UTC), domain.SexUnknown)
	if err != nil {
		t.Fatal(err)
	}

	base := time.Date(2026, 9, 6, 11, 0, 0, 0, time.UTC)
	for i := 0; i < readings; i++ {
		if _, err := postgres.NewMeasurements(pool).Create(ctx, postgres.NewMeasurement{
			PatientID:  patient.ID,
			Type:       "HEART_RATE",
			Value:      60 + float64(i%13),
			Unit:       "bpm",
			RecordedAt: base.Add(time.Duration(i) * time.Second),
			Source:     "integration",
		}); err != nil {
			t.Fatalf("seed measurement %d: %v", i, err)
		}
	}

	tokens := auth.NewTokens(cfg.Auth.JWT, nil)
	token, _, err := tokens.Issue(operator.ID, operator.Role)
	if err != nil {
		t.Fatalf("issue token: %v", err)
	}

	return &processingHarness{
		app: a, stub: stub, pool: pool, token: token,
		patientID: patient.ID, userID: operator.ID, logs: logs,
		jobs:    postgres.NewJobs(pool),
		results: postgres.NewResults(pool),
		audit:   postgres.NewAudit(pool),
		ctx:     ctx,
	}
}

func (h *processingHarness) do(t *testing.T, method, path, body string, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	var req *http.Request
	if body != "" {
		req = httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
	} else {
		req = httptest.NewRequest(method, path, nil)
	}
	req.Header.Set("Authorization", "Bearer "+h.token)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	h.app.Handler().ServeHTTP(rec, req)
	return rec
}

func (h *processingHarness) createBody() string {
	return fmt.Sprintf(`{"patient_id":%q,"measurement_types":["HEART_RATE"],"windows":["1h"],"percentiles":[50,95]}`, h.patientID)
}

func decodeJob(t *testing.T, rec *httptest.ResponseRecorder) model.ProcessingJob {
	t.Helper()
	var job model.ProcessingJob
	if err := json.Unmarshal(rec.Body.Bytes(), &job); err != nil {
		t.Fatalf("decode job (%d): %s", rec.Code, rec.Body.String())
	}
	return job
}

func decodeErrorBody(t *testing.T, rec *httptest.ResponseRecorder) model.ErrorResponse {
	t.Helper()
	var body model.ErrorResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode error (%d): %s", rec.Code, rec.Body.String())
	}
	return body
}

// ------------------------------------------------------- successful run

func TestProcessingJobSucceedsAndPersistsEverything(t *testing.T) {
	h := newProcessingHarness(t, 5, nil)
	h.stub.succeed()

	rec := h.do(t, http.MethodPost, "/api/v1/processing/jobs", h.createBody(), map[string]string{
		requestid.Header:            "req-integration",
		requestid.CorrelationHeader: "corr-integration",
	})
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201: %s", rec.Code, rec.Body.String())
	}
	job := decodeJob(t, rec)
	if job.Status != string(domain.JobCompleted) {
		t.Fatalf("status = %s, want COMPLETED", job.Status)
	}
	if job.AttemptCount != 1 {
		t.Errorf("attempt_count = %d, want 1", job.AttemptCount)
	}
	if job.ServiceVersion == nil || *job.ServiceVersion != "processor-stub" {
		t.Errorf("service_version = %v, want the processor's own", job.ServiceVersion)
	}
	if job.ErrorCode != nil {
		t.Errorf("a completed job carries no error: %v", job.ErrorCode)
	}
	if got := rec.Header().Get("Location"); !strings.HasSuffix(got, job.ID.String()) {
		t.Errorf("Location = %q", got)
	}

	// The row really moved through its lifecycle.
	stored, err := h.jobs.GetByID(h.ctx, job.ID)
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	if stored.Status != domain.JobCompleted {
		t.Errorf("stored status = %s", stored.Status)
	}
	if stored.StartedAt == nil || stored.CompletedAt == nil {
		t.Error("a completed job must carry both timestamps")
	}
	if stored.RequestID == nil || *stored.RequestID != "req-integration" {
		t.Errorf("request id = %v, want the client's", stored.RequestID)
	}
	// The correlation id is what ties this job to the client's request
	// across both services.
	if stored.TraceID == nil || *stored.TraceID != "corr-integration" {
		t.Errorf("correlation id = %v", stored.TraceID)
	}

	// The results were written in the same transaction as the transition.
	results, err := h.results.ListByJob(h.ctx, job.ID, nil, 100)
	if err != nil {
		t.Fatalf("ListByJob: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("stored %d results, want 1", len(results))
	}
	if results[0].PatientID != h.patientID {
		t.Error("the result must be attributed to the patient by the database")
	}

	// The dispatch carried the readings and the correlation headers.
	body := h.stub.lastBody.Load()
	var sent processing.Request
	if err := json.Unmarshal(*body, &sent); err != nil {
		t.Fatalf("the dispatch is not a processing request: %v", err)
	}
	if len(sent.Readings) != 5 {
		t.Errorf("dispatched %d readings, want 5", len(sent.Readings))
	}
	head := h.stub.lastHead.Load()
	if head.Get(requestid.CorrelationHeader) != "corr-integration" {
		t.Errorf("the correlation id did not reach the processor: %q", head.Get(requestid.CorrelationHeader))
	}
	if head.Get("Authorization") == "" {
		t.Error("the processor was called without a credential")
	}

	// The creation was audited.
	entries, err := h.audit.ListByResource(h.ctx, job.ID, 10)
	if err != nil {
		t.Fatalf("audit: %v", err)
	}
	if len(entries) != 1 || entries[0].Action != domain.AuditProcessingJobCreated {
		t.Fatalf("audit entries = %+v, want one PROCESSING_JOB_CREATED", entries)
	}
	if entries[0].RequestID != "req-integration" {
		t.Errorf("audit request id = %q", entries[0].RequestID)
	}
}

func TestProcessingJobIsReadableAndItsResultsAreListed(t *testing.T) {
	h := newProcessingHarness(t, 3, nil)
	h.stub.succeed()

	created := decodeJob(t, h.do(t, http.MethodPost, "/api/v1/processing/jobs",
		fmt.Sprintf(`{"patient_id":%q,"measurement_types":["HEART_RATE"],"windows":["1m","1h"]}`, h.patientID), nil))

	got := h.do(t, http.MethodGet, "/api/v1/processing/jobs/"+created.ID.String(), "", nil)
	if got.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", got.Code, got.Body.String())
	}
	if decodeJob(t, got).ID != created.ID {
		t.Error("the wrong job came back")
	}

	list := h.do(t, http.MethodGet, "/api/v1/patients/"+h.patientID.String()+"/processing-results?limit=1", "", nil)
	if list.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", list.Code, list.Body.String())
	}
	var page model.Page[model.ProcessingResult]
	if err := json.Unmarshal(list.Body.Bytes(), &page); err != nil {
		t.Fatalf("decode page: %v", err)
	}
	if len(page.Items) != 1 {
		t.Fatalf("returned %d items, want the page size", len(page.Items))
	}
	if !page.HasMore || page.NextCursor == nil {
		t.Fatal("two windows were requested, so a second page must be offered")
	}

	next := h.do(t, http.MethodGet,
		"/api/v1/patients/"+h.patientID.String()+"/processing-results?limit=1&cursor="+*page.NextCursor, "", nil)
	var second model.Page[model.ProcessingResult]
	if err := json.Unmarshal(next.Body.Bytes(), &second); err != nil {
		t.Fatalf("decode second page: %v", err)
	}
	if len(second.Items) != 1 || second.Items[0].ID == page.Items[0].ID {
		t.Error("the second page must continue after the first")
	}
}

// --------------------------------------------------- Rust unavailable

func TestAJobSurvivesAProcessorThatIsNotThere(t *testing.T) {
	h := newProcessingHarness(t, 3, nil)
	// Nothing answers: the stub's default is a 503, and the client will
	// exhaust its attempts.
	h.stub.fail(http.StatusServiceUnavailable, "PROCESSOR_OVERLOADED")

	rec := h.do(t, http.MethodPost, "/api/v1/processing/jobs", h.createBody(), nil)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503: %s", rec.Code, rec.Body.String())
	}
	body := decodeErrorBody(t, rec)
	if body.Error.Code != processing.CodeProcessorUnavailable {
		t.Errorf("code = %s", body.Error.Code)
	}
	if strings.Contains(body.Error.Message, "pod-9") {
		t.Errorf("the processor's own words reached the client: %q", body.Error.Message)
	}
	if got := h.stub.requests.Load(); got != 3 {
		t.Errorf("made %d attempts, want the configured 3", got)
	}

	// The job must not have disappeared: it is FAILED with its diagnosis.
	jobs, err := h.jobs.ListByPatient(h.ctx, h.patientID, nil, 10)
	if err != nil {
		t.Fatalf("ListByPatient: %v", err)
	}
	if len(jobs) != 1 {
		t.Fatalf("stored %d jobs, want the failed one kept", len(jobs))
	}
	if jobs[0].Status != domain.JobFailed {
		t.Errorf("status = %s, want FAILED", jobs[0].Status)
	}
	if jobs[0].ErrorCode == nil || *jobs[0].ErrorCode != processing.CodeProcessorUnavailable {
		t.Errorf("error_code = %v", jobs[0].ErrorCode)
	}
	if jobs[0].AttemptCount != 1 {
		t.Errorf("attempt_count = %d, want the one dispatch counted", jobs[0].AttemptCount)
	}
	if jobs[0].FailedAt == nil || jobs[0].StartedAt == nil {
		t.Error("a failed job carries both timestamps")
	}
}

// A transient failure must leave no idempotency record, or the client could
// never retry the request.
func TestATransientFailureLeavesTheIdempotencyKeyFree(t *testing.T) {
	h := newProcessingHarness(t, 3, nil)
	h.stub.fail(http.StatusServiceUnavailable, "PROCESSOR_OVERLOADED")
	key := map[string]string{idempotency.Header: "retry-me-please"}

	if rec := h.do(t, http.MethodPost, "/api/v1/processing/jobs", h.createBody(), key); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rec.Code)
	}

	// The processor recovers and the same key is used again.
	h.stub.succeed()
	rec := h.do(t, http.MethodPost, "/api/v1/processing/jobs", h.createBody(), key)
	if rec.Code != http.StatusCreated {
		t.Fatalf("the retry was refused: %d %s", rec.Code, rec.Body.String())
	}
	if rec.Header().Get(middleware.ReplayedHeader) != "" {
		t.Error("the retry must run, not replay the failure")
	}
	if decodeJob(t, rec).Status != string(domain.JobCompleted) {
		t.Error("the retry should have completed")
	}
}

// A completed request is replayed rather than run twice, so a client that
// repeats a call does not create a second job.
func TestARepeatedRequestReplaysInsteadOfCreatingASecondJob(t *testing.T) {
	h := newProcessingHarness(t, 3, nil)
	h.stub.succeed()
	key := map[string]string{idempotency.Header: "only-once"}

	first := h.do(t, http.MethodPost, "/api/v1/processing/jobs", h.createBody(), key)
	if first.Code != http.StatusCreated {
		t.Fatalf("status = %d: %s", first.Code, first.Body.String())
	}
	dispatches := h.stub.requests.Load()

	second := h.do(t, http.MethodPost, "/api/v1/processing/jobs", h.createBody(), key)
	if second.Code != http.StatusCreated {
		t.Fatalf("replay status = %d", second.Code)
	}
	if second.Header().Get(middleware.ReplayedHeader) != "true" {
		t.Error("the repeat must be marked as replayed")
	}
	if decodeJob(t, first).ID != decodeJob(t, second).ID {
		t.Error("the replay must return the same job")
	}
	if h.stub.requests.Load() != dispatches {
		t.Error("the replay must not dispatch again")
	}

	jobs, err := h.jobs.ListByPatient(h.ctx, h.patientID, nil, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(jobs) != 1 {
		t.Errorf("created %d jobs, want exactly one", len(jobs))
	}
}

// ---------------------------------------------------------- timeout

func TestAProcessorThatNeverAnswersTimesOut(t *testing.T) {
	h := newProcessingHarness(t, 3, map[string]string{"PROCESSOR_TIMEOUT": "100ms"})
	h.stub.answer(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-time.After(3 * time.Second):
		}
	})

	rec := h.do(t, http.MethodPost, "/api/v1/processing/jobs", h.createBody(), nil)
	if rec.Code != http.StatusGatewayTimeout {
		t.Fatalf("status = %d, want 504: %s", rec.Code, rec.Body.String())
	}
	if got := decodeErrorBody(t, rec).Error.Code; got != processing.CodeProcessorTimeout {
		t.Errorf("code = %s", got)
	}

	jobs, err := h.jobs.ListByPatient(h.ctx, h.patientID, nil, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(jobs) != 1 || jobs[0].Status != domain.JobFailed {
		t.Fatalf("job = %+v, want one FAILED", jobs)
	}
	if jobs[0].ErrorCode == nil || *jobs[0].ErrorCode != processing.CodeProcessorTimeout {
		t.Errorf("error_code = %v", jobs[0].ErrorCode)
	}
}

// ------------------------------------------------- malformed response

func TestAMalformedProcessorResponseStoresNothing(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{"not JSON", `<html>oops</html>`},
		{"a result of another job", `{"job_id":"11111111-1111-4111-8111-111111111111",
			"algorithm_version":"1.0.0","service_version":"v","accepted":1,"skipped":0,
			"rejected":[],"results":[]}`},
		{"a field the gateway does not know", `{"job_id":"x","algorithm_version":"1.0.0",
			"service_version":"v","accepted":1,"skipped":0,"rejected":[],"results":[],"surprise":1}`},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newProcessingHarness(t, 3, nil)
			h.stub.answer(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, tc.body)
			})

			rec := h.do(t, http.MethodPost, "/api/v1/processing/jobs", h.createBody(), nil)
			if rec.Code != http.StatusServiceUnavailable {
				t.Fatalf("status = %d, want 503: %s", rec.Code, rec.Body.String())
			}
			if got := decodeErrorBody(t, rec).Error.Code; got != processing.CodeProcessorProtocol {
				t.Errorf("code = %s, want %s", got, processing.CodeProcessorProtocol)
			}

			jobs, err := h.jobs.ListByPatient(h.ctx, h.patientID, nil, 10)
			if err != nil {
				t.Fatal(err)
			}
			if len(jobs) != 1 || jobs[0].Status != domain.JobFailed {
				t.Fatalf("job = %+v, want one FAILED", jobs)
			}
			results, err := h.results.ListByJob(h.ctx, jobs[0].ID, nil, 10)
			if err != nil {
				t.Fatal(err)
			}
			if len(results) != 0 {
				t.Errorf("stored %d results from an answer the gateway cannot trust", len(results))
			}
		})
	}
}

// An outcome carrying a result the schema would refuse must not leave the
// job COMPLETED with some of its results missing.
func TestAResultTheSchemaRefusesFailsTheWholeJob(t *testing.T) {
	h := newProcessingHarness(t, 3, nil)
	h.stub.answer(func(w http.ResponseWriter, r *http.Request) {
		body := h.stub.lastBody.Load()
		var req processing.Request
		_ = json.Unmarshal(*body, &req)
		w.Header().Set("Content-Type", "application/json")
		// The second result names a measurement type the catalogue does not
		// hold, which the database rejects.
		_, _ = fmt.Fprintf(w, `{"job_id":%q,"algorithm_version":"1.0.0","service_version":"v",
			"accepted":3,"skipped":0,"rejected":[],"results":[
			{"job_id":%q,"measurement_type":"HEART_RATE","window":"1h","window_start":"2026-09-06T11:00:00Z",
			 "statistics":{"count":1},"anomalies":[],"algorithm_version":"1.0.0","service_version":"v"},
			{"job_id":%q,"measurement_type":"PULSE","window":"1h","window_start":"2026-09-06T11:00:00Z",
			 "statistics":{"count":1},"anomalies":[],"algorithm_version":"1.0.0","service_version":"v"}]}`,
			req.Job.ID, req.Job.ID, req.Job.ID)
	})

	rec := h.do(t, http.MethodPost, "/api/v1/processing/jobs", h.createBody(), nil)
	if rec.Code < 500 {
		t.Fatalf("status = %d, want a server-side failure: %s", rec.Code, rec.Body.String())
	}

	jobs, err := h.jobs.ListByPatient(h.ctx, h.patientID, nil, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(jobs) != 1 {
		t.Fatalf("stored %d jobs", len(jobs))
	}
	if jobs[0].Status == domain.JobCompleted {
		t.Error("a job whose results could not all be stored must not be COMPLETED")
	}
	results, err := h.results.ListByJob(h.ctx, jobs[0].ID, nil, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 0 {
		t.Errorf("stored %d results, want none: the batch is all or nothing", len(results))
	}
}

// ------------------------------------------------ partial processing

// The processor reports how many readings it accepted, skipped and
// rejected. A reading the gateway sent and the processor would not process
// means the statistics cover less data than was asked for, so reporting the
// job COMPLETED would present a partial answer as a whole one.
func TestAPartiallyProcessedJobIsNotReportedAsComplete(t *testing.T) {
	h := newProcessingHarness(t, 5, nil)
	// Three of the five readings were refused, for instance because their
	// timestamps fall outside the processor's acceptance window.
	h.stub.answer(func(w http.ResponseWriter, r *http.Request) {
		body := h.stub.lastBody.Load()
		var req processing.Request
		_ = json.Unmarshal(*body, &req)
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"job_id":%q,"algorithm_version":"1.0.0","service_version":"processor-stub",
			"accepted":2,"skipped":0,
			"rejected":[{"index":2,"code":"TIMESTAMP_OUT_OF_BOUNDS","reason":"too old"},
			            {"index":3,"code":"TIMESTAMP_OUT_OF_BOUNDS","reason":"too old"},
			            {"index":4,"code":"TIMESTAMP_OUT_OF_BOUNDS","reason":"too old"}],
			"results":[{"job_id":%q,"measurement_type":"HEART_RATE","window":"1h",
			 "window_start":"2026-09-06T11:00:00Z","statistics":{"count":2},"anomalies":[],
			 "algorithm_version":"1.0.0","service_version":"processor-stub"}]}`, req.Job.ID, req.Job.ID)
	})

	rec := h.do(t, http.MethodPost, "/api/v1/processing/jobs", h.createBody(), nil)
	if rec.Code == http.StatusCreated {
		job := decodeJob(t, rec)
		if job.Status == string(domain.JobCompleted) {
			t.Fatal("a job whose readings were only partly processed was reported COMPLETED; " +
				"its statistics cover less data than was asked for")
		}
	}
	if rec.Code < 400 {
		t.Fatalf("status = %d, want the partial processing reported as a failure: %s", rec.Code, rec.Body.String())
	}

	jobs, err := h.jobs.ListByPatient(h.ctx, h.patientID, nil, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(jobs) != 1 || jobs[0].Status != domain.JobFailed {
		t.Fatalf("job = %+v, want one FAILED", jobs)
	}
	results, err := h.results.ListByJob(h.ctx, jobs[0].ID, nil, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 0 {
		t.Errorf("stored %d results computed over a truncated dataset", len(results))
	}
}

// A reading of a type the job did not request should never be sent, so the
// processor skipping one means the gateway sent something it should not
// have.
func TestSkippedReadingsAreAlsoTreatedAsAPartialResult(t *testing.T) {
	h := newProcessingHarness(t, 4, nil)
	h.stub.answer(func(w http.ResponseWriter, r *http.Request) {
		body := h.stub.lastBody.Load()
		var req processing.Request
		_ = json.Unmarshal(*body, &req)
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"job_id":%q,"algorithm_version":"1.0.0","service_version":"processor-stub",
			"accepted":3,"skipped":1,"rejected":[],
			"results":[{"job_id":%q,"measurement_type":"HEART_RATE","window":"1h",
			 "window_start":"2026-09-06T11:00:00Z","statistics":{"count":3},"anomalies":[],
			 "algorithm_version":"1.0.0","service_version":"processor-stub"}]}`, req.Job.ID, req.Job.ID)
	})

	rec := h.do(t, http.MethodPost, "/api/v1/processing/jobs", h.createBody(), nil)
	if rec.Code < 400 {
		t.Fatalf("status = %d, want a failure: %s", rec.Code, rec.Body.String())
	}
}

// ------------------------------------------------------ nothing to do

func TestAJobWithNoMeasurementsFailsAndIsRecorded(t *testing.T) {
	h := newProcessingHarness(t, 0, nil)
	h.stub.succeed()

	rec := h.do(t, http.MethodPost, "/api/v1/processing/jobs", h.createBody(), nil)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422: %s", rec.Code, rec.Body.String())
	}
	if got := decodeErrorBody(t, rec).Error.Code; got != processing.CodeNoMeasurements {
		t.Errorf("code = %s", got)
	}
	if h.stub.requests.Load() != 0 {
		t.Error("the processor must not be called when there is nothing to send")
	}

	jobs, err := h.jobs.ListByPatient(h.ctx, h.patientID, nil, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(jobs) != 1 || jobs[0].Status != domain.JobFailed {
		t.Fatalf("job = %+v, want one FAILED", jobs)
	}
}

func TestAJobLargerThanTheBoundIsRefused(t *testing.T) {
	h := newProcessingHarness(t, 6, map[string]string{"PROCESSING_MAX_JOB_MEASUREMENTS": "5"})
	h.stub.succeed()

	rec := h.do(t, http.MethodPost, "/api/v1/processing/jobs", h.createBody(), nil)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422: %s", rec.Code, rec.Body.String())
	}
	if got := decodeErrorBody(t, rec).Error.Code; got != processing.CodeJobTooLarge {
		t.Errorf("code = %s", got)
	}
	if h.stub.requests.Load() != 0 {
		t.Error("an oversized job must be refused before it is sent")
	}
}

// ----------------------------------------------------- authorization

func TestProcessingRoutesAreGuarded(t *testing.T) {
	h := newProcessingHarness(t, 1, nil)
	h.stub.succeed()

	for _, tc := range []struct{ method, path string }{
		{http.MethodPost, "/api/v1/processing/jobs"},
		{http.MethodGet, "/api/v1/processing/jobs/" + uuid.New().String()},
		{http.MethodGet, "/api/v1/patients/" + h.patientID.String() + "/processing-results"},
	} {
		req := httptest.NewRequest(tc.method, tc.path, strings.NewReader(h.createBody()))
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		h.app.Handler().ServeHTTP(rec, req)
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("%s %s without a token = %d, want 401", tc.method, tc.path, rec.Code)
		}
	}
}

func TestAnUnknownJobAndAnUnknownPatientAreNotFound(t *testing.T) {
	h := newProcessingHarness(t, 1, nil)

	if rec := h.do(t, http.MethodGet, "/api/v1/processing/jobs/"+uuid.New().String(), "", nil); rec.Code != http.StatusNotFound {
		t.Errorf("unknown job = %d, want 404", rec.Code)
	}
	if rec := h.do(t, http.MethodGet, "/api/v1/patients/"+uuid.New().String()+"/processing-results", "", nil); rec.Code != http.StatusNotFound {
		t.Errorf("unknown patient = %d, want 404", rec.Code)
	}
	body := fmt.Sprintf(`{"patient_id":%q,"measurement_types":["HEART_RATE"],"windows":["1h"]}`, uuid.New())
	if rec := h.do(t, http.MethodPost, "/api/v1/processing/jobs", body, nil); rec.Code != http.StatusNotFound {
		t.Errorf("unknown patient on create = %d, want 404", rec.Code)
	}
}

func TestAnInvalidJobRequestIsRejectedBeforeAnythingIsWritten(t *testing.T) {
	h := newProcessingHarness(t, 1, nil)
	h.stub.succeed()

	body := fmt.Sprintf(`{"patient_id":%q,"measurement_types":["PULSE"],"windows":["3h"]}`, h.patientID)
	rec := h.do(t, http.MethodPost, "/api/v1/processing/jobs", body, nil)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422: %s", rec.Code, rec.Body.String())
	}
	got := decodeErrorBody(t, rec)
	if got.Error.Code != processing.CodeValidationFailed {
		t.Errorf("code = %s", got.Error.Code)
	}
	if len(got.Error.Details) < 2 {
		t.Errorf("details = %+v, want every problem named", got.Error.Details)
	}

	jobs, err := h.jobs.ListByPatient(h.ctx, h.patientID, nil, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(jobs) != 0 {
		t.Errorf("an invalid request created %d jobs", len(jobs))
	}
}
