//go:build e2e

// Package e2e runs the whole system: the real Go gateway, a real
// PostgreSQL, and the real Rust processor as a separate process speaking
// HTTP over a real socket (SPECIFICATIONS.md section 48).
//
// Nothing here is stubbed. The unit tests check each service's own
// behaviour, the contract tests check that both sides agree on the shape,
// and this checks that they actually work together: a job created through
// the public API is processed by the Rust engine and its results come back
// through the public API.
//
// The processor binary is located through PROCESSOR_BINARY. Without it the
// suite skips, so a developer who has not built the processor still gets a
// green run and CI, which builds both services, gets the real thing.
package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/n0ah-n0wa/VitalMesh/services/api-gateway/internal/app"
	"github.com/n0ah-n0wa/VitalMesh/services/api-gateway/internal/auth"
	"github.com/n0ah-n0wa/VitalMesh/services/api-gateway/internal/config"
	"github.com/n0ah-n0wa/VitalMesh/services/api-gateway/internal/domain"
	"github.com/n0ah-n0wa/VitalMesh/services/api-gateway/internal/httpapi/model"
	"github.com/n0ah-n0wa/VitalMesh/services/api-gateway/internal/infra/postgres"
	"github.com/n0ah-n0wa/VitalMesh/services/api-gateway/internal/infra/postgres/postgrestest"
	"github.com/n0ah-n0wa/VitalMesh/services/api-gateway/internal/observability/logging"
	"github.com/n0ah-n0wa/VitalMesh/services/api-gateway/internal/processing"
)

const internalToken = "e2e-internal-token-not-a-secret"

// syncBuffer collects a child process's output. os/exec writes it from
// goroutines of its own while the test goroutine reads it to report a
// failure, so every access is guarded; a plain bytes.Buffer is a data race.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// processor is the real Rust service, running as a child process.
type processor struct {
	cmd      *exec.Cmd
	addr     string
	logs     *syncBuffer
	stopOnce sync.Once
}

// startProcessor builds nothing: it runs the binary the build produced.
func startProcessor(t *testing.T, env map[string]string) *processor {
	t.Helper()
	binary := os.Getenv("PROCESSOR_BINARY")
	if binary == "" {
		t.Skip("PROCESSOR_BINARY is not set; build the processor and set it to run the end-to-end suite")
	}
	if _, err := os.Stat(binary); err != nil {
		t.Skipf("PROCESSOR_BINARY %q is not usable: %v", binary, err)
	}

	addr := freeAddr(t)
	logs := &syncBuffer{}
	cmd := exec.Command(binary)
	cmd.Env = append(os.Environ(),
		"HTTP_ADDR="+addr,
		"ENVIRONMENT=test",
		"LOG_FORMAT=json",
		"LOG_LEVEL=warn",
		"INTERNAL_TOKEN="+internalToken,
	)
	for k, v := range env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	cmd.Stdout = logs
	cmd.Stderr = logs
	if err := cmd.Start(); err != nil {
		t.Fatalf("start the processor: %v", err)
	}

	p := &processor{cmd: cmd, addr: addr, logs: logs}
	t.Cleanup(p.stop)
	p.waitReady(t)
	return p
}

func (p *processor) url() string { return "http://" + p.addr }

func (p *processor) stop() {
	p.stopOnce.Do(func() {
		if p.cmd.Process == nil {
			return
		}
		_ = p.cmd.Process.Kill()
		// Wait, not Process.Wait: it also waits for the goroutines copying
		// the child's output, so nothing writes to the log buffer after
		// this returns.
		_ = p.cmd.Wait()
	})
}

// waitReady polls the processor's own health endpoint until it answers.
func (p *processor) waitReady(t *testing.T) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	client := &http.Client{Timeout: time.Second}
	for time.Now().Before(deadline) {
		res, err := client.Get(p.url() + "/internal/v1/health")
		if err == nil {
			_ = res.Body.Close()
			if res.StatusCode == http.StatusOK {
				return
			}
		}
		if p.cmd.ProcessState != nil {
			t.Fatalf("the processor exited before it was ready:\n%s", p.logs.String())
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("the processor did not become ready:\n%s", p.logs.String())
}

func freeAddr(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve a port: %v", err)
	}
	addr := l.Addr().String()
	_ = l.Close()
	return addr
}

// system is a gateway wired to a processor and a database.
type system struct {
	gateway   *app.App
	processor *processor
	token     string
	patientID uuid.UUID
	jobs      *postgres.Jobs
	ctx       context.Context
	// logs is everything the gateway wrote, so a test can join what the
	// gateway recorded with what the processor recorded.
	logs *bytes.Buffer
}

func newSystem(t *testing.T, readings int, processorURL string) *system {
	t.Helper()
	pool, dbURL, _ := postgrestest.New(t)

	cfg, err := config.Load(func(key string) (string, bool) {
		switch key {
		case "DATABASE_URL":
			return dbURL, true
		case "JWT_SECRET":
			return "e2e-secret-e2e-secret-e2e-secret", true
		case "PASSWORD_HASH_MEMORY_KIB":
			return "8192", true
		case "PASSWORD_HASH_TIME":
			return "1", true
		case "PROCESSOR_URL":
			return processorURL, true
		case "PROCESSOR_TOKEN":
			return internalToken, true
		case "PROCESSOR_TIMEOUT":
			return "10s", true
		case "PROCESSOR_BACKOFF":
			return "10ms", true
		case "PROCESSOR_MAX_BACKOFF":
			return "50ms", true
		case "HTTP_REQUEST_TIMEOUT":
			return "30s", true
		case "HTTP_WRITE_TIMEOUT":
			return "40s", true
		}
		return "", false
	})
	if err != nil {
		t.Fatalf("config: %v", err)
	}

	logs := &bytes.Buffer{}
	gateway, err := app.New(context.Background(), cfg,
		logging.New(logs, config.Log{Format: config.LogFormatJSON}, logging.Service{Name: "e2e"}), "v-e2e")
	if err != nil {
		t.Fatalf("wire the gateway: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	t.Cleanup(cancel)

	hash, err := auth.NewHasher(cfg.Auth.Password).Hash(ctx, "e2e-password")
	if err != nil {
		t.Fatal(err)
	}
	operator, err := postgres.NewUsers(pool).Create(ctx, "e2e@example.com", hash, domain.RoleOperator)
	if err != nil {
		t.Fatal(err)
	}
	patient, err := postgres.NewPatients(pool).Create(ctx, "e2e-patient",
		time.Date(1980, 1, 1, 0, 0, 0, 0, time.UTC), domain.SexUnknown)
	if err != nil {
		t.Fatal(err)
	}

	// Heart-rate readings at one-second intervals, with one value well above
	// the healthy range so the engine has something to find. The series
	// *ends* two hours ago, so however many readings there are they all sit
	// inside the processor's acceptance window, which runs from
	// `requested_at - MAX_MEASUREMENT_AGE` to `requested_at + MAX_FUTURE_SKEW`.
	// A series that ran forward from a fixed start would cross the future
	// bound once it was long enough, and the processor would refuse its tail.
	base := time.Now().UTC().
		Add(-2 * time.Hour).
		Add(-time.Duration(readings) * time.Second).
		Truncate(time.Second)
	items := make([]postgres.NewMeasurement, readings)
	for i := range items {
		value := 60 + float64(i%7)
		if i == readings/2 {
			value = 195
		}
		items[i] = postgres.NewMeasurement{
			PatientID:  patient.ID,
			Type:       "HEART_RATE",
			Value:      value,
			Unit:       "bpm",
			RecordedAt: base.Add(time.Duration(i) * time.Second),
			Source:     "e2e",
		}
	}
	if readings > 0 {
		if _, err := postgres.NewMeasurements(pool).CreateBatch(ctx, items); err != nil {
			t.Fatalf("seed measurements: %v", err)
		}
	}

	token, _, err := auth.NewTokens(cfg.Auth.JWT, nil).Issue(operator.ID, operator.Role)
	if err != nil {
		t.Fatalf("issue token: %v", err)
	}

	return &system{
		gateway: gateway, token: token, patientID: patient.ID,
		jobs: postgres.NewJobs(pool), ctx: ctx, logs: logs,
	}
}

func (s *system) do(t *testing.T, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	var req *http.Request
	if body != "" {
		req = httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
	} else {
		req = httptest.NewRequest(method, path, nil)
	}
	req.Header.Set("Authorization", "Bearer "+s.token)
	rec := httptest.NewRecorder()
	s.gateway.Handler().ServeHTTP(rec, req)
	return rec
}

// jobRequest is the body of a straightforward job, for tests that care
// about what travels with a request rather than about the work itself.
func (s *system) jobRequest(t *testing.T) string {
	t.Helper()
	return fmt.Sprintf(`{"patient_id":%q,"measurement_types":["HEART_RATE"],
		"windows":["1m"],"percentiles":[50,95]}`, s.patientID)
}

func (s *system) createJob(t *testing.T, windows string) *httptest.ResponseRecorder {
	t.Helper()
	body := fmt.Sprintf(`{"patient_id":%q,"measurement_types":["HEART_RATE"],
		"windows":[%s],"percentiles":[50,95]}`, s.patientID, windows)
	return s.do(t, http.MethodPost, "/api/v1/processing/jobs", body)
}

func decodeJob(t *testing.T, rec *httptest.ResponseRecorder) model.ProcessingJob {
	t.Helper()
	var job model.ProcessingJob
	if err := json.Unmarshal(rec.Body.Bytes(), &job); err != nil {
		t.Fatalf("decode job (%d): %s", rec.Code, rec.Body.String())
	}
	return job
}

// ------------------------------------------------- successful processing

// The whole flow: measurements in PostgreSQL, a job created through the
// public API, dispatched over HTTP to the Rust engine, and real statistics
// coming back through the public API.
func TestAJobIsProcessedByTheRealEngine(t *testing.T) {
	p := startProcessor(t, map[string]string{
		// A rule set, so the engine has something to flag.
		"ANOMALY_RULES": `{"rules":[{"name":"hr-bounds","kind":"THRESHOLD",
			"measurement_types":["HEART_RATE"],
			"tiers":{"warning":{"lower":40,"upper":180}}}]}`,
	})
	s := newSystem(t, 60, p.url())

	rec := s.createJob(t, `"1m","1h"`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201: %s\nprocessor logs:\n%s", rec.Code, rec.Body.String(), p.logs.String())
	}
	job := decodeJob(t, rec)
	if job.Status != string(domain.JobCompleted) {
		t.Fatalf("status = %s, want COMPLETED (error %v: %v)", job.Status, job.ErrorCode, job.ErrorMessage)
	}
	if job.ServiceVersion == nil || *job.ServiceVersion == "" {
		t.Error("the job must record which processor build produced it")
	}
	if job.AlgorithmVersion != config.DefaultAlgorithmVersion {
		t.Errorf("algorithm version = %q", job.AlgorithmVersion)
	}

	// The job is readable through the API.
	got := decodeJob(t, s.do(t, http.MethodGet, "/api/v1/processing/jobs/"+job.ID.String(), ""))
	if got.ID != job.ID || got.Status != string(domain.JobCompleted) {
		t.Errorf("the job did not read back: %+v", got)
	}

	// The results are real statistics computed by the Rust engine.
	list := s.do(t, http.MethodGet, "/api/v1/patients/"+s.patientID.String()+"/processing-results?limit=100", "")
	if list.Code != http.StatusOK {
		t.Fatalf("results status = %d: %s", list.Code, list.Body.String())
	}
	var page model.Page[model.ProcessingResult]
	if err := json.Unmarshal(list.Body.Bytes(), &page); err != nil {
		t.Fatalf("decode results: %v", err)
	}
	if len(page.Items) == 0 {
		t.Fatal("the engine produced no results")
	}

	windows := map[string]bool{}
	anomalies := 0
	for _, res := range page.Items {
		windows[res.Window] = true
		if res.JobID != job.ID {
			t.Errorf("result %s belongs to another job", res.ID)
		}
		if res.MeasurementType != "HEART_RATE" {
			t.Errorf("result type = %s", res.MeasurementType)
		}
		var stats struct {
			Count       int                `json:"count"`
			Min         float64            `json:"min"`
			Max         float64            `json:"max"`
			Mean        float64            `json:"mean"`
			Median      float64            `json:"median"`
			StdDev      float64            `json:"std_dev"`
			Percentiles map[string]float64 `json:"percentiles"`
		}
		if err := json.Unmarshal(res.Statistics, &stats); err != nil {
			t.Fatalf("statistics are not usable: %v (%s)", err, res.Statistics)
		}
		if stats.Count < 1 {
			t.Errorf("result %s has count %d", res.ID, stats.Count)
		}
		if stats.Min > stats.Mean || stats.Mean > stats.Max {
			t.Errorf("the mean is outside [min, max]: %+v", stats)
		}
		if _, ok := stats.Percentiles["50"]; !ok {
			t.Errorf("the requested percentiles are missing: %s", res.Statistics)
		}
		var flagged []map[string]any
		if err := json.Unmarshal(res.Anomalies, &flagged); err != nil {
			t.Fatalf("anomalies are not usable: %v", err)
		}
		anomalies += len(flagged)
	}
	if !windows["1m"] || !windows["1h"] {
		t.Errorf("windows produced = %v, want both requested", windows)
	}
	// The reading of 195 bpm is above the rule's upper bound of 180.
	if anomalies == 0 {
		t.Error("the engine flagged nothing, though one reading is outside the configured bounds")
	}

	// The hour window covers every reading.
	for _, res := range page.Items {
		if res.Window != "1h" {
			continue
		}
		var stats struct {
			Count int     `json:"count"`
			Max   float64 `json:"max"`
		}
		_ = json.Unmarshal(res.Statistics, &stats)
		if stats.Count != 60 {
			t.Errorf("the hour window counted %d readings, want 60", stats.Count)
		}
		if stats.Max != 195 {
			t.Errorf("the hour window's maximum is %v, want the outlier", stats.Max)
		}
	}
}

// ------------------------------------------------------ Rust unavailable

// With the processor stopped, the gateway must fail safely: a clear 503, a
// job row that survives with its diagnosis, and nothing hanging.
func TestTheGatewayFailsSafelyWhenTheProcessorIsGone(t *testing.T) {
	p := startProcessor(t, nil)
	s := newSystem(t, 10, p.url())

	// It works while the processor is up.
	if rec := s.createJob(t, `"1h"`); rec.Code != http.StatusCreated {
		t.Fatalf("the first job failed: %d %s", rec.Code, rec.Body.String())
	}

	p.stop()

	start := time.Now()
	rec := s.createJob(t, `"1h"`)
	elapsed := time.Since(start)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503: %s", rec.Code, rec.Body.String())
	}
	var body model.ErrorResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode error: %v", err)
	}
	if body.Error.Code != processing.CodeProcessorUnavailable {
		t.Errorf("code = %s, want %s", body.Error.Code, processing.CodeProcessorUnavailable)
	}
	if elapsed > 25*time.Second {
		t.Errorf("took %v: the request must fail promptly, not hang", elapsed)
	}

	// Both jobs are on record: the one that worked and the one that did not.
	jobs, err := s.jobs.ListByPatient(s.ctx, s.patientID, nil, 10)
	if err != nil {
		t.Fatalf("ListByPatient: %v", err)
	}
	if len(jobs) != 2 {
		t.Fatalf("stored %d jobs, want both attempts kept", len(jobs))
	}
	var completed, failed int
	for _, j := range jobs {
		switch j.Status {
		case domain.JobCompleted:
			completed++
		case domain.JobFailed:
			failed++
			if j.ErrorCode == nil || *j.ErrorCode != processing.CodeProcessorUnavailable {
				t.Errorf("the failed job's code = %v", j.ErrorCode)
			}
			if j.FailedAt == nil {
				t.Error("a failed job carries the instant it failed")
			}
		}
	}
	if completed != 1 || failed != 1 {
		t.Errorf("completed = %d, failed = %d, want one of each", completed, failed)
	}

	// The rest of the API is unaffected: one service being down must not
	// take the gateway with it.
	if rec := s.do(t, http.MethodGet, "/api/v1/patients/"+s.patientID.String()+"/measurements", ""); rec.Code != http.StatusOK {
		t.Errorf("measurements = %d, want the gateway to keep serving", rec.Code)
	}
	if rec := s.do(t, http.MethodGet, "/health", ""); rec.Code != http.StatusOK {
		t.Errorf("health = %d, want the gateway to stay live", rec.Code)
	}
}

// ------------------------------------------------------------- timeout

// A processor that answers too slowly must produce a timeout, not a hang.
// The job's own bound is set below the gateway's so the engine stops the
// work itself.
func TestASlowProcessorTimesOut(t *testing.T) {
	p := startProcessor(t, map[string]string{
		// The engine refuses to spend more than a millisecond on a job.
		"PROCESSING_TIMEOUT": "1ms",
	})
	// Enough work that it cannot finish inside that millisecond on any
	// machine: five thousand readings over all seven windows take tens of
	// milliseconds even in a release build. A smaller job would make the
	// test a race between the engine and its own timer, which a fast
	// runner wins.
	s := newSystem(t, 5000, p.url())

	rec := s.createJob(t, `"1m","5m","15m","1h","6h","24h","7d"`)
	if rec.Code != http.StatusGatewayTimeout {
		t.Fatalf("status = %d, want 504: %s\nprocessor logs:\n%s", rec.Code, rec.Body.String(), p.logs.String())
	}
	var body model.ErrorResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode error: %v", err)
	}
	if body.Error.Code != processing.CodeProcessorTimeout {
		t.Errorf("code = %s, want %s", body.Error.Code, processing.CodeProcessorTimeout)
	}

	jobs, err := s.jobs.ListByPatient(s.ctx, s.patientID, nil, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(jobs) != 1 || jobs[0].Status != domain.JobFailed {
		t.Fatalf("jobs = %+v, want one FAILED", jobs)
	}
	if jobs[0].ErrorCode == nil || *jobs[0].ErrorCode != processing.CodeProcessorTimeout {
		t.Errorf("error code = %v", jobs[0].ErrorCode)
	}
}

// --------------------------------------------------------- credentials

// The processor refuses a gateway that presents the wrong credential, and
// the gateway must not describe that to its own client.
func TestAWrongInternalCredentialIsAProcessorFailureNotAClientError(t *testing.T) {
	p := startProcessor(t, map[string]string{"INTERNAL_TOKEN": "a-different-token-entirely"})
	s := newSystem(t, 10, p.url())

	rec := s.createJob(t, `"1h"`)
	if rec.Code < 500 {
		t.Fatalf("status = %d, want a server-side failure: %s", rec.Code, rec.Body.String())
	}
	var body model.ErrorResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode error: %v", err)
	}
	if body.Error.Code != processing.CodeProcessorProtocol {
		t.Errorf("code = %s, want %s", body.Error.Code, processing.CodeProcessorProtocol)
	}
	lower := strings.ToLower(body.Error.Message)
	if strings.Contains(lower, "token") || strings.Contains(lower, "auth") {
		t.Errorf("the message tells a client about the internal credential: %q", body.Error.Message)
	}
}

// ------------------------------------------------------ size agreement

// The two services advertise the same job ceiling, so a job at any size up
// to it must actually go through. A job of ten thousand readings is well
// under the advertised hundred thousand, and is the size at which the two
// defaults stop agreeing: the readings weigh more than the processor's
// default body limit.
func TestAJobWellUnderTheAdvertisedCeilingIsAccepted(t *testing.T) {
	p := startProcessor(t, nil)
	s := newSystem(t, 10000, p.url())

	rec := s.createJob(t, `"1h"`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("a job of 10000 readings failed with %d: %s\nThe services advertise a ceiling of %d "+
			"readings, so a job this size must be accepted.\nprocessor logs:\n%s",
			rec.Code, rec.Body.String(), 100000, p.logs.String())
	}
	if job := decodeJob(t, rec); job.Status != string(domain.JobCompleted) {
		t.Fatalf("status = %s (error %v)", job.Status, job.ErrorCode)
	}
}

// --------------------------------------------------- version agreement

// The processor the gateway is talking to must implement the contract and
// the algorithm version this gateway was built against.
func TestTheTwoServicesAgreeOnTheirVersions(t *testing.T) {
	p := startProcessor(t, nil)

	res, err := http.Get(p.url() + "/internal/v1/health")
	if err != nil {
		t.Fatalf("health: %v", err)
	}
	defer res.Body.Close()

	var health struct {
		AlgorithmVersion string `json:"algorithm_version"`
		ContractVersion  string `json:"contract_version"`
		Limits           struct {
			MaxJobMeasurements int `json:"max_job_measurements"`
		} `json:"limits"`
	}
	if err := json.NewDecoder(res.Body).Decode(&health); err != nil {
		t.Fatalf("decode health: %v", err)
	}
	if health.AlgorithmVersion != config.DefaultAlgorithmVersion {
		t.Errorf("the processor implements algorithm version %q but the gateway asks for %q",
			health.AlgorithmVersion, config.DefaultAlgorithmVersion)
	}
	if health.ContractVersion != config.InternalContractVersion {
		t.Errorf("the processor implements contract %q but the gateway was built against %q",
			health.ContractVersion, config.InternalContractVersion)
	}
	// The gateway must not send more than the processor will take.
	cfg, err := config.Load(func(key string) (string, bool) {
		if key == "DATABASE_URL" {
			return "postgres://x/y?sslmode=disable", true
		}
		if key == "JWT_SECRET" {
			return "e2e-secret-e2e-secret-e2e-secret", true
		}
		return "", false
	})
	if err != nil {
		t.Fatalf("config: %v", err)
	}
	if cfg.Processing.MaxJobMeasurements > health.Limits.MaxJobMeasurements {
		t.Errorf("the gateway would send up to %d readings but the processor accepts %d",
			cfg.Processing.MaxJobMeasurements, health.Limits.MaxJobMeasurements)
	}
}

// ------------------------------------------------------------- tracing

// traceIDs returns every distinct trace id in a JSON log stream.
func traceIDs(t *testing.T, logs string) map[string]bool {
	t.Helper()
	found := map[string]bool{}
	for _, line := range strings.Split(logs, "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var record map[string]any
		if err := json.Unmarshal([]byte(line), &record); err != nil {
			continue // a line the service wrote before its logger was ready
		}
		if id, ok := record["trace_id"].(string); ok && id != "" {
			found[id] = true
		}
	}
	return found
}

// The whole point of the phase: one request produces one trace across both
// services. The gateway records a trace id, injects W3C trace context into
// its call, and the processor records the same id, which is what lets an
// operator follow a request from the client to the engine and back.
func TestOneTraceSpansTheGatewayAndTheProcessor(t *testing.T) {
	p := startProcessor(t, map[string]string{"LOG_LEVEL": "info"})
	s := newSystem(t, 200, p.url())

	rec := s.do(t, http.MethodPost, "/api/v1/processing/jobs", s.jobRequest(t))
	if rec.Code != http.StatusCreated {
		t.Fatalf("create a job = %d: %s", rec.Code, rec.Body.String())
	}

	gateway := traceIDs(t, s.logs.String())
	if len(gateway) == 0 {
		t.Fatalf("the gateway recorded no trace id:\n%s", s.logs.String())
	}
	processor := traceIDs(t, p.logs.String())
	if len(processor) == 0 {
		t.Fatalf("the processor recorded no trace id:\n%s", p.logs.String())
	}

	shared := false
	for id := range processor {
		if gateway[id] {
			shared = true
			break
		}
	}
	if !shared {
		t.Errorf("the two services are on different traces\ngateway: %v\nprocessor: %v", gateway, processor)
	}
}

// A trace that starts in a client must be the trace both services join,
// rather than each starting one of their own. This is what W3C trace
// context is for.
func TestAClientsTraceIsContinuedByBothServices(t *testing.T) {
	const traceID = "4bf92f3577b34da6a3ce929d0e0e4736"

	p := startProcessor(t, map[string]string{"LOG_LEVEL": "info"})
	s := newSystem(t, 200, p.url())

	req := httptest.NewRequest(http.MethodPost, "/api/v1/processing/jobs", strings.NewReader(s.jobRequest(t)))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+s.token)
	req.Header.Set("traceparent", "00-"+traceID+"-00f067aa0ba902b7-01")
	rec := httptest.NewRecorder()
	s.gateway.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create a job = %d: %s", rec.Code, rec.Body.String())
	}

	if !traceIDs(t, s.logs.String())[traceID] {
		t.Errorf("the gateway started its own trace instead of continuing the client's:\n%s", s.logs.String())
	}
	if !traceIDs(t, p.logs.String())[traceID] {
		t.Errorf("the processor did not continue the client's trace:\n%s", p.logs.String())
	}
}

// Correlation identifiers travel alongside trace context and must survive
// the hop too, because they are what a human quotes in a ticket.
func TestCorrelationIdentifiersReachTheProcessor(t *testing.T) {
	const correlation = "corr-e2e-12345"

	p := startProcessor(t, map[string]string{"LOG_LEVEL": "info"})
	s := newSystem(t, 200, p.url())

	req := httptest.NewRequest(http.MethodPost, "/api/v1/processing/jobs", strings.NewReader(s.jobRequest(t)))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+s.token)
	req.Header.Set("X-Correlation-ID", correlation)
	rec := httptest.NewRecorder()
	s.gateway.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create a job = %d: %s", rec.Code, rec.Body.String())
	}

	if !strings.Contains(p.logs.String(), correlation) {
		t.Errorf("the correlation id did not reach the processor:\n%s", p.logs.String())
	}
	if got := rec.Header().Get("X-Correlation-ID"); got != correlation {
		t.Errorf("the response carried correlation id %q, want %q", got, correlation)
	}
}

// No identifier of a patient and no reading may appear in what the
// processor records for a traced request: a span leaves the process for a
// backend that is not the record's custodian.
func TestATracedRequestLeaksNoPayloadToTheProcessorsRecords(t *testing.T) {
	p := startProcessor(t, map[string]string{"LOG_LEVEL": "info"})
	s := newSystem(t, 200, p.url())

	rec := s.do(t, http.MethodPost, "/api/v1/processing/jobs", s.jobRequest(t))
	if rec.Code != http.StatusCreated {
		t.Fatalf("create a job = %d: %s", rec.Code, rec.Body.String())
	}

	logs := p.logs.String()
	if strings.Contains(logs, s.patientID.String()) {
		t.Errorf("the patient id reached the processor's records:\n%s", logs)
	}
	// The route pattern is recorded, never the path that carried the id.
	if !strings.Contains(logs, `"route":"/internal/v1/process"`) {
		t.Errorf("the processor did not record the route pattern:\n%s", logs)
	}
}
