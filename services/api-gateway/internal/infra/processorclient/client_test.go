package processorclient

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/n0ah-n0wa/VitalMesh/services/api-gateway/internal/config"
	"github.com/n0ah-n0wa/VitalMesh/services/api-gateway/internal/domain"
	"github.com/n0ah-n0wa/VitalMesh/services/api-gateway/internal/processing"
	"github.com/n0ah-n0wa/VitalMesh/services/api-gateway/internal/requestid"
)

const testToken = "example-processor-token"

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func testConfig(baseURL string) config.Processor {
	return config.Processor{
		BaseURL:         baseURL,
		Token:           config.Secret(testToken),
		Timeout:         2 * time.Second,
		MaxAttempts:     3,
		Backoff:         time.Millisecond,
		MaxBackoff:      2 * time.Millisecond,
		ContractVersion: config.InternalContractVersion,
	}
}

// newClient returns a client whose sleeps are instant, so a retry test does
// not spend the backoff it is asserting on.
func newClient(t *testing.T, baseURL string, tune ...func(*config.Processor)) (*Client, *[]time.Duration) {
	t.Helper()
	cfg := testConfig(baseURL)
	for _, f := range tune {
		f(&cfg)
	}
	var slept []time.Duration
	c := New(cfg, discardLogger(), Options{
		Sleep: func(_ context.Context, d time.Duration) error {
			slept = append(slept, d)
			return nil
		},
	})
	return c, &slept
}

func request() processing.Request {
	return processing.Request{
		Job: processing.RequestJob{
			ID:        "11111111-1111-4111-8111-111111111111",
			PatientID: "22222222-2222-4222-8222-222222222222",
			Parameters: processing.RequestParams{
				MeasurementTypes: []string{"HEART_RATE"},
				Windows:          []string{"1h"},
				Percentiles:      []int{50},
			},
			AlgorithmVersion: "1.0.0",
			Status:           domain.JobPending,
			RequestedAt:      "2026-09-06T12:00:00Z",
		},
		Readings: []processing.Reading{{
			ID: "33333333-3333-4333-8333-333333333333", Type: "HEART_RATE",
			Value: 72, Unit: "bpm", RecordedAt: "2026-09-06T11:00:00Z",
		}},
	}
}

func outcomeBody(jobID string) string {
	return fmt.Sprintf(`{
		"job_id": %q,
		"algorithm_version": "1.0.0",
		"service_version": "processor-1",
		"accepted": 1,
		"skipped": 0,
		"rejected": [],
		"results": [{
			"job_id": %q,
			"measurement_type": "HEART_RATE",
			"window": "1h",
			"window_start": "2026-09-06T11:00:00Z",
			"statistics": {"count": 1},
			"anomalies": [],
			"algorithm_version": "1.0.0",
			"service_version": "processor-1"
		}]
	}`, jobID, jobID)
}

func errorBody(code string) string {
	return fmt.Sprintf(`{"error":{"code":%q,"message":"something operational","request_id":"r","retryable":true}}`, code)
}

func processorError(t *testing.T, err error) *processing.ProcessorError {
	t.Helper()
	var pe *processing.ProcessorError
	if !errors.As(err, &pe) {
		t.Fatalf("error %v is not a processor error", err)
	}
	return pe
}

// ---------------------------------------------------------------- success

func TestProcessReturnsTheOutcome(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, outcomeBody(request().Job.ID))
	}))
	defer srv.Close()

	c, _ := newClient(t, srv.URL)
	out, err := c.Process(context.Background(), request())
	if err != nil {
		t.Fatalf("Process: %v", err)
	}
	if out.JobID != request().Job.ID {
		t.Errorf("job id = %q", out.JobID)
	}
	if out.ServiceVersion != "processor-1" {
		t.Errorf("service version = %q", out.ServiceVersion)
	}
	if len(out.Results) != 1 || out.Results[0].Window != "1h" {
		t.Errorf("results = %+v", out.Results)
	}
}

func TestProcessSendsTheCredentialAndTheCorrelationHeaders(t *testing.T) {
	var got http.Header
	var body []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Clone()
		body, _ = io.ReadAll(r.Body)
		_, _ = io.WriteString(w, outcomeBody(request().Job.ID))
	}))
	defer srv.Close()

	ctx := requestid.NewContext(context.Background(), "req-42")
	ctx = requestid.NewCorrelationContext(ctx, "corr-99")
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()

	c, _ := newClient(t, srv.URL)
	if _, err := c.Process(ctx, request()); err != nil {
		t.Fatalf("Process: %v", err)
	}

	if got.Get("Authorization") != "Bearer "+testToken {
		t.Errorf("Authorization = %q", got.Get("Authorization"))
	}
	if got.Get(requestid.Header) != "req-42" {
		t.Errorf("request id = %q", got.Get(requestid.Header))
	}
	if got.Get(requestid.CorrelationHeader) != "corr-99" {
		t.Errorf("correlation id = %q", got.Get(requestid.CorrelationHeader))
	}
	if got.Get("Content-Type") != "application/json" {
		t.Errorf("content type = %q", got.Get("Content-Type"))
	}
	// The processor is told what is left of the attempt, so it can stop work
	// nobody is waiting for.
	budget, err := strconv.Atoi(got.Get("X-Request-Timeout-Ms"))
	if err != nil || budget < 1 || budget > 3000 {
		t.Errorf("X-Request-Timeout-Ms = %q, want the remaining budget", got.Get("X-Request-Timeout-Ms"))
	}
	// The body must be the contract's request, not something re-shaped.
	var sent map[string]any
	if err := json.Unmarshal(body, &sent); err != nil {
		t.Fatalf("the body is not JSON: %v", err)
	}
	if _, ok := sent["job"]; !ok {
		t.Errorf("the body has no job: %s", body)
	}
	if _, ok := sent["readings"]; !ok {
		t.Errorf("the body has no readings: %s", body)
	}
}

// ---------------------------------------------------------------- retries

func TestProcessRetriesARetryableFailureAndThenSucceeds(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) < 3 {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = io.WriteString(w, errorBody("PROCESSOR_OVERLOADED"))
			return
		}
		_, _ = io.WriteString(w, outcomeBody(request().Job.ID))
	}))
	defer srv.Close()

	c, slept := newClient(t, srv.URL)
	if _, err := c.Process(context.Background(), request()); err != nil {
		t.Fatalf("Process: %v", err)
	}
	if got := calls.Load(); got != 3 {
		t.Errorf("made %d attempts, want 3", got)
	}
	// The delay doubles: one base wait, then twice the base.
	if len(*slept) != 2 || (*slept)[1] <= (*slept)[0] {
		t.Errorf("backoff = %v, want it to grow", *slept)
	}
}

func TestProcessStopsAtTheAttemptBound(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = io.WriteString(w, errorBody("PROCESSOR_OVERLOADED"))
	}))
	defer srv.Close()

	c, _ := newClient(t, srv.URL)
	_, err := c.Process(context.Background(), request())
	pe := processorError(t, err)
	if pe.Code != processing.CodeProcessorUnavailable {
		t.Errorf("code = %s", pe.Code)
	}
	if got := calls.Load(); got != 3 {
		t.Errorf("made %d attempts, want exactly the configured 3", got)
	}
}

func TestProcessHonoursRetryAfter(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			w.Header().Set("Retry-After", "1")
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = io.WriteString(w, errorBody("PROCESSOR_OVERLOADED"))
			return
		}
		_, _ = io.WriteString(w, outcomeBody(request().Job.ID))
	}))
	defer srv.Close()

	// The cap is raised so the asked-for delay is what the client waits.
	c, slept := newClient(t, srv.URL, func(c *config.Processor) { c.MaxBackoff = 5 * time.Second })
	if _, err := c.Process(context.Background(), request()); err != nil {
		t.Fatalf("Process: %v", err)
	}
	if len(*slept) != 1 || (*slept)[0] != time.Second {
		t.Errorf("waited %v, want the second the processor asked for", *slept)
	}
}

func TestProcessCapsTheDelayItWasAskedFor(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "3600")
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = io.WriteString(w, errorBody("PROCESSOR_OVERLOADED"))
	}))
	defer srv.Close()

	c, slept := newClient(t, srv.URL)
	if _, err := c.Process(context.Background(), request()); err == nil {
		t.Fatal("Process should have failed")
	}
	for _, d := range *slept {
		if d > 2*time.Millisecond {
			t.Errorf("waited %v, longer than the configured cap", d)
		}
	}
}

func TestProcessDoesNotRetryWhenTheDeadlineLeavesNoRoom(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = io.WriteString(w, errorBody("PROCESSOR_OVERLOADED"))
	}))
	defer srv.Close()

	c, _ := newClient(t, srv.URL)
	// A deadline that has almost passed: sleeping into it and being
	// cancelled would waste what is left of the caller's budget.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()

	_, err := c.Process(ctx, request())
	if err == nil {
		t.Fatal("Process should have failed")
	}
	if got := calls.Load(); got != 1 {
		t.Errorf("made %d attempts, want 1: there was no time for another", got)
	}
}

// ------------------------------------------------- retry classification

func TestProcessClassifiesEveryFailureTheContractDefines(t *testing.T) {
	cases := []struct {
		name         string
		status       int
		code         string
		wantCode     string
		wantKind     domain.Kind
		wantAttempts int
	}{
		{"overloaded", 503, "PROCESSOR_OVERLOADED", processing.CodeProcessorUnavailable, domain.KindUnavailable, 3},
		{"shutting down", 503, "PROCESSOR_SHUTTING_DOWN", processing.CodeProcessorUnavailable, domain.KindUnavailable, 3},
		{"cancelled", 503, "PROCESSING_CANCELLED", processing.CodeProcessorUnavailable, domain.KindUnavailable, 1},
		{"timeout", 504, "PROCESSING_TIMEOUT", processing.CodeProcessorTimeout, domain.KindTimeout, 3},
		{"internal", 500, "INTERNAL_ERROR", processing.CodeProcessorProtocol, domain.KindUnavailable, 3},
		{"no valid measurements", 422, "NO_VALID_MEASUREMENTS", processing.CodeNoMeasurements, domain.KindValidation, 1},
		{"job too large", 422, "JOB_TOO_LARGE", processing.CodeJobTooLarge, domain.KindValidation, 1},
		{"unsupported version", 422, "UNSUPPORTED_ALGORITHM_VERSION", processing.CodeProcessorRejected, domain.KindValidation, 1},
		{"unauthenticated", 401, "UNAUTHENTICATED", processing.CodeProcessorProtocol, domain.KindUnavailable, 1},
		{"bad request", 400, "INVALID_REQUEST", processing.CodeProcessorProtocol, domain.KindUnavailable, 1},
		{"unsupported media", 415, "UNSUPPORTED_MEDIA_TYPE", processing.CodeProcessorProtocol, domain.KindUnavailable, 1},
		{"body too large", 413, "REQUEST_BODY_TOO_LARGE", processing.CodeProcessorProtocol, domain.KindUnavailable, 1},
		// A duplicate is the processor still working on this gateway's own
		// previous attempt, which ended when that attempt timed out. It is
		// transient, not a disagreement between the services.
		{"duplicate job", 409, "JOB_ALREADY_RUNNING", processing.CodeProcessorBusy, domain.KindUnavailable, 3},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var calls atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				w.WriteHeader(tc.status)
				_, _ = io.WriteString(w, errorBody(tc.code))
			}))
			defer srv.Close()

			c, _ := newClient(t, srv.URL)
			_, err := c.Process(context.Background(), request())
			pe := processorError(t, err)
			if pe.Code != tc.wantCode {
				t.Errorf("code = %s, want %s", pe.Code, tc.wantCode)
			}
			if pe.Kind != tc.wantKind {
				t.Errorf("kind = %s, want %s", pe.Kind, tc.wantKind)
			}
			if got := int(calls.Load()); got != tc.wantAttempts {
				t.Errorf("made %d attempts, want %d", got, tc.wantAttempts)
			}
		})
	}
}

// The processor writes its messages for its own operators; they can name
// internal limits and hosts and must never reach a client of the gateway.
func TestAFailureNeverCarriesTheProcessorsMessage(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnprocessableEntity)
		_, _ = io.WriteString(w, `{"error":{"code":"NO_VALID_MEASUREMENTS",
			"message":"none of the 3 readings can be processed at pod-7 shard 4","request_id":"r"}}`)
	}))
	defer srv.Close()

	c, _ := newClient(t, srv.URL)
	_, err := c.Process(context.Background(), request())
	pe := processorError(t, err)
	if strings.Contains(pe.Message, "pod-7") || strings.Contains(pe.Message, "shard") {
		t.Errorf("the client-facing message repeated the processor's: %q", pe.Message)
	}
	if pe.Err == nil {
		t.Error("the cause must be kept for the log")
	}
}

// ----------------------------------------------------------- unavailable

func TestProcessReportsAProcessorThatIsNotThere(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	addr := srv.URL
	srv.Close() // nothing is listening now

	c, _ := newClient(t, addr)
	_, err := c.Process(context.Background(), request())
	pe := processorError(t, err)
	if pe.Code != processing.CodeProcessorUnavailable {
		t.Errorf("code = %s, want %s", pe.Code, processing.CodeProcessorUnavailable)
	}
	if pe.Kind != domain.KindUnavailable {
		t.Errorf("kind = %s", pe.Kind)
	}
	if !pe.Retryable {
		t.Error("a refused connection is worth retrying")
	}
}

func TestProcessBoundsEachAttemptWithItsOwnTimeout(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		// Outlast the client's per-attempt bound without outlasting the
		// test, so Close never waits on a handler.
		select {
		case <-r.Context().Done():
		case <-time.After(time.Second):
		}
	}))
	defer srv.Close()

	// A short per-attempt bound, and a caller deadline long enough for
	// every attempt, so it is the attempt bound that fires.
	c, _ := newClient(t, srv.URL, func(c *config.Processor) { c.Timeout = 60 * time.Millisecond })
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	start := time.Now()
	_, err := c.Process(ctx, request())
	elapsed := time.Since(start)

	pe := processorError(t, err)
	if pe.Code != processing.CodeProcessorTimeout {
		t.Errorf("code = %s, want %s", pe.Code, processing.CodeProcessorTimeout)
	}
	if pe.Kind != domain.KindTimeout {
		t.Errorf("kind = %s", pe.Kind)
	}
	if got := calls.Load(); got != 3 {
		t.Errorf("made %d attempts, want the timeout to be retried", got)
	}
	if elapsed > 3*time.Second {
		t.Errorf("took %v: the per-attempt bound was not applied", elapsed)
	}
}

func TestProcessStopsWhenTheCallerGivesUp(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-time.After(time.Second):
		}
	}))
	defer srv.Close()

	c, _ := newClient(t, srv.URL, func(c *config.Processor) { c.Timeout = 5 * time.Second })
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(30 * time.Millisecond)
		cancel()
	}()

	_, err := c.Process(ctx, request())
	pe := processorError(t, err)
	if pe.Retryable {
		t.Error("a caller that gave up must not trigger a retry")
	}
	if pe.Kind != domain.KindTimeout {
		t.Errorf("kind = %s", pe.Kind)
	}
}

// ----------------------------------------------------- malformed answers

func TestAnUnusableAnswerIsAProtocolFailureAndIsNotRetried(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{"not JSON", `not json at all`},
		{"truncated", `{"job_id": "x"`},
		{"empty", ``},
		{"two values", `{"job_id":"x","algorithm_version":"1.0.0","service_version":"v","accepted":0,
			"skipped":0,"rejected":[],"results":[]} {"job_id":"y"}`},
		{"an array", `[]`},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var calls atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				_, _ = io.WriteString(w, tc.body)
			}))
			defer srv.Close()

			c, _ := newClient(t, srv.URL)
			_, err := c.Process(context.Background(), request())
			pe := processorError(t, err)
			if pe.Code != processing.CodeProcessorProtocol {
				t.Errorf("code = %s, want %s", pe.Code, processing.CodeProcessorProtocol)
			}
			if pe.Retryable {
				t.Error("repeating a call that produced nonsense produces nonsense")
			}
			if got := calls.Load(); got != 1 {
				t.Errorf("made %d attempts, want 1", got)
			}
		})
	}
}

// Adding a response field is a compatible change under the contract's
// versioning rules, so a processor that starts sending one must not break
// this gateway.
func TestAnOutcomeWithAFieldThisGatewayDoesNotKnowIsStillUsable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = fmt.Fprintf(w, `{
			"job_id": %q,
			"algorithm_version": "1.0.0",
			"service_version": "processor-1",
			"accepted": 1, "skipped": 0, "rejected": [],
			"results": [{"job_id": %q, "measurement_type": "HEART_RATE", "window": "1h",
			  "window_start": "2026-09-06T11:00:00Z", "statistics": {"count": 1}, "anomalies": [],
			  "algorithm_version": "1.0.0", "service_version": "processor-1",
			  "confidence": 0.99}],
			"engine_notes": ["something a later version added"]
		}`, request().Job.ID, request().Job.ID)
	}))
	defer srv.Close()

	c, _ := newClient(t, srv.URL)
	out, err := c.Process(context.Background(), request())
	if err != nil {
		t.Fatalf("a compatible addition broke the client: %v", err)
	}
	if out.Accepted != 1 || len(out.Results) != 1 {
		t.Fatalf("the known fields did not survive: %+v", out)
	}
	if out.Results[0].Window != "1h" {
		t.Errorf("window = %q", out.Results[0].Window)
	}
}

func TestAnOutcomeForAnotherJobIsRefused(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, outcomeBody("99999999-9999-4999-8999-999999999999"))
	}))
	defer srv.Close()

	c, _ := newClient(t, srv.URL)
	_, err := c.Process(context.Background(), request())
	pe := processorError(t, err)
	if pe.Code != processing.CodeProcessorProtocol {
		t.Errorf("code = %s, want the mismatch refused", pe.Code)
	}
}

func TestAFailureWithAnUnreadableBodyIsStillClassifiedByItsStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = io.WriteString(w, `<html>gateway says no</html>`)
	}))
	defer srv.Close()

	c, _ := newClient(t, srv.URL)
	_, err := c.Process(context.Background(), request())
	pe := processorError(t, err)
	if pe.Code != processing.CodeProcessorUnavailable {
		t.Errorf("code = %s, want the status to decide", pe.Code)
	}
	if !pe.Retryable {
		t.Error("a 503 is retryable whatever its body")
	}
}

// A processor answering with something enormous must not be able to
// exhaust the gateway's memory.
func TestAnOversizedAnswerIsRefused(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"job_id":"`)
		chunk := strings.Repeat("a", 1<<20)
		for written := 0; written <= MaxResponseBytes; written += len(chunk) {
			if _, err := io.WriteString(w, chunk); err != nil {
				return
			}
		}
		_, _ = io.WriteString(w, `"}`)
	}))
	defer srv.Close()

	c, _ := newClient(t, srv.URL)
	_, err := c.Process(context.Background(), request())
	pe := processorError(t, err)
	if pe.Code != processing.CodeProcessorProtocol {
		t.Errorf("code = %s", pe.Code)
	}
}

// ----------------------------------------------------------------- health

func TestHealthReadsTheVersionsAndLimits(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != healthPath {
			t.Errorf("path = %s, want %s", r.URL.Path, healthPath)
		}
		if r.Header.Get("Authorization") != "" {
			t.Error("the health endpoint needs no credential")
		}
		_, _ = io.WriteString(w, `{"status":"ok","service":"processor","version":"dev",
			"algorithm_version":"1.0.0","contract_version":"1.1.1",
			"limits":{"max_job_measurements":100000,"max_request_bytes":1048576,
			"processing_timeout_ms":300000,"max_concurrent_jobs":4,"job_retention_seconds":900}}`)
	}))
	defer srv.Close()

	c, _ := newClient(t, srv.URL)
	health, err := c.Health(context.Background())
	if err != nil {
		t.Fatalf("Health: %v", err)
	}
	if health.AlgorithmVersion != "1.0.0" {
		t.Errorf("algorithm version = %q", health.AlgorithmVersion)
	}
	if health.ContractVersion != config.InternalContractVersion {
		t.Errorf("contract version = %q, want the one this gateway was built against", health.ContractVersion)
	}
	if health.Limits.MaxJobMeasurements != 100000 {
		t.Errorf("limits = %+v", health.Limits)
	}
}

func TestHealthReportsAProcessorThatIsNotThere(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	addr := srv.URL
	srv.Close()

	c, _ := newClient(t, addr)
	if _, err := c.Health(context.Background()); err == nil {
		t.Fatal("Health should have failed")
	}
}

// ------------------------------------------------------------- base URL

func TestValidateBaseURL(t *testing.T) {
	for _, ok := range []string{"http://processor:8081", "https://processor.example", "http://127.0.0.1:8081"} {
		if err := ValidateBaseURL(ok); err != nil {
			t.Errorf("%q rejected: %v", ok, err)
		}
	}
	for _, bad := range []string{"", "processor:8081", "ftp://processor", "http://"} {
		if err := ValidateBaseURL(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

// A trailing slash in configuration must not produce a double slash in the
// path, which some servers route differently.
func TestABaseURLWithATrailingSlashStillAddressesTheRightPath(t *testing.T) {
	var path string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path = r.URL.Path
		_, _ = io.WriteString(w, outcomeBody(request().Job.ID))
	}))
	defer srv.Close()

	c, _ := newClient(t, srv.URL+"/")
	if _, err := c.Process(context.Background(), request()); err != nil {
		t.Fatalf("Process: %v", err)
	}
	if path != processPath {
		t.Errorf("path = %q, want %q", path, processPath)
	}
}
