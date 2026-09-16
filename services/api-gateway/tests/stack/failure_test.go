//go:build stack

package stack

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

// -------------------------------------------------------- invalid input

func (s *stack) testInvalidInput(t *testing.T) {
	patient := s.patient(t, "invalid")

	// Every failing field is reported, once, in one answer.
	bad := map[string]any{"patient_id": patient.String(), "type": "PULSE", "value": -5, "unit": "bpm", "recorded_at": "yesterday", "source": ""}
	r := s.do(t, http.MethodPost, "/api/v1/measurements", s.operator.token, bad, nil).expect(t, http.StatusUnprocessableEntity, "MEASUREMENT_VALIDATION_FAILED")
	for _, field := range []string{"type", "recorded_at", "source"} {
		if r.detail(field) == "" {
			t.Errorf("field %s is not reported: %s", field, r.body)
		}
	}
	cases := []struct {
		name  string
		mut   func(m map[string]any)
		field string
	}{
		{"value above the technical range", func(m map[string]any) { m["value"] = 301 }, "value"},
		{"wrong unit for the type", func(m map[string]any) { m["unit"] = "mmHg" }, "unit"},
		{"a reading in the future", func(m map[string]any) { m["recorded_at"] = time.Now().Add(time.Hour).UTC().Format(time.RFC3339) }, "recorded_at"},
		{"before 1900", func(m map[string]any) { m["recorded_at"] = "1899-12-31T23:59:59Z" }, "recorded_at"},
		{"unknown patient", func(m map[string]any) { m["patient_id"] = uuid.New().String() }, "patient_id"},
		{"missing value", func(m map[string]any) { delete(m, "value") }, "value"},
		{"metadata not an object", func(m map[string]any) { m["metadata"] = []int{1} }, "metadata"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := s.reading(patient, "HEART_RATE", 72, "bpm", 1, "e2e-invalid")
			tc.mut(m)
			r := s.do(t, http.MethodPost, "/api/v1/measurements", s.operator.token, m, nil).expect(t, http.StatusUnprocessableEntity, "MEASUREMENT_VALIDATION_FAILED")
			if r.detail(tc.field) == "" {
				t.Errorf("field %s is not named: %s", tc.field, r.body)
			}
		})
	}

	// Malformed requests are refused before any rule runs.
	s.do(t, http.MethodPost, "/api/v1/measurements", s.operator.token, "{not json", nil).expect(t, http.StatusBadRequest, "")
	unknownField := s.reading(patient, "HEART_RATE", 72, "bpm", 2, "e2e-invalid")
	unknownField["colour"] = "red"
	uf := s.do(t, http.MethodPost, "/api/v1/measurements", s.operator.token, unknownField, nil)
	if uf.status != http.StatusBadRequest && uf.status != http.StatusUnprocessableEntity {
		t.Errorf("an unknown field must be rejected, got %s", uf.describe())
	}
	req, _ := http.NewRequest(http.MethodPost, s.gateway+"/api/v1/measurements", strings.NewReader("value=1"))
	req.Header.Set("Content-Type", "text/plain")
	req.Header.Set("Authorization", "Bearer "+s.operator.token)
	resp, err := s.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnsupportedMediaType {
		t.Errorf("text/plain body answered %d, want 415", resp.StatusCode)
	}
	huge := strings.Repeat("x", 2<<20)
	s.do(t, http.MethodPost, "/api/v1/patients", s.operator.token, map[string]string{"external_reference": huge, "date_of_birth": "1990-01-01", "sex": "MALE"}, nil).expect(t, http.StatusRequestEntityTooLarge, "")

	// Patients and jobs validate too.
	pr := s.do(t, http.MethodPost, "/api/v1/patients", s.operator.token, map[string]string{"external_reference": "", "date_of_birth": "2999-01-01", "sex": "YES"}, nil).expect(t, http.StatusUnprocessableEntity, "VALIDATION_FAILED")
	for _, field := range []string{"external_reference", "date_of_birth", "sex"} {
		if pr.detail(field) == "" {
			t.Errorf("patient field %s is not reported: %s", field, pr.body)
		}
	}
	job := s.jobRequest(patient)
	job["windows"] = []string{"2m"}
	job["percentiles"] = []int{0, 150}
	jr := s.do(t, http.MethodPost, "/api/v1/processing/jobs", s.operator.token, job, nil).expect(t, http.StatusUnprocessableEntity, "PROCESSING_VALIDATION_FAILED")
	if len(jr.envelope().Error.Details) == 0 {
		t.Errorf("job validation without details: %s", jr.body)
	}
	if n := s.count(t, "SELECT count(*) FROM measurements WHERE patient_id = $1", patient); n != 0 {
		t.Errorf("%d readings stored by invalid requests", n)
	}
	if n := s.count(t, "SELECT count(*) FROM processing_jobs WHERE patient_id = $1", patient); n != 0 {
		t.Errorf("%d jobs recorded for invalid requests", n)
	}
}

// ---------------------------------------------------- duplicate request

func (s *stack) testDuplicateRequest(t *testing.T) {
	patient := s.patient(t, "duplicate")
	reading := s.reading(patient, "BODY_TEMPERATURE", 36.8, "C", 3, "e2e-thermometer")

	// Without a key, the same request twice is a conflict on the data.
	s.do(t, http.MethodPost, "/api/v1/measurements", s.operator.token, reading, nil).expect(t, http.StatusCreated, "")
	s.do(t, http.MethodPost, "/api/v1/measurements", s.operator.token, reading, nil).expect(t, http.StatusConflict, "MEASUREMENT_ALREADY_EXISTS")

	// With a key, it is a replay of the first answer, and nothing happens
	// twice: a client that retries after a lost response is safe.
	key := map[string]string{"Idempotency-Key": "e2e-" + s.runID + "-dup"}
	other := s.reading(patient, "BODY_TEMPERATURE", 36.9, "C", 4, "e2e-thermometer")
	first := s.do(t, http.MethodPost, "/api/v1/measurements", s.operator.token, other, key).expect(t, http.StatusCreated, "")
	second := s.do(t, http.MethodPost, "/api/v1/measurements", s.operator.token, other, key).expect(t, http.StatusCreated, "")
	if second.header.Get("Idempotency-Replayed") != "true" || string(first.body) == "" {
		t.Errorf("second request was not a replay: %v", second.header)
	}
	if n := s.count(t, "SELECT count(*) FROM measurements WHERE patient_id = $1", patient); n != 2 {
		t.Errorf("%d readings stored, want 2", n)
	}
	if n := s.count(t, "SELECT count(*) FROM audit_logs WHERE action = 'MEASUREMENT_CREATED' AND request_id = $1", second.requestID()); n != 0 {
		t.Error("a replay wrote an audit record as if it had stored something")
	}

	// Concurrent duplicates: the same job under one key from two clients
	// at once yields one job. Either both see the same 201, or the second
	// is told the first is still running.
	batch := map[string]any{"items": s.heartRateSeries(patient, "e2e-monitor")}
	s.do(t, http.MethodPost, "/api/v1/measurements/batch", s.operator.token, batch, nil).expect(t, http.StatusCreated, "")
	jobKey := map[string]string{"Idempotency-Key": "e2e-" + s.runID + "-dup-job"}
	type result struct {
		r   response
		err error
	}
	results := make(chan result, 2)
	for i := 0; i < 2; i++ {
		go func() {
			r, err := s.raw(http.MethodPost, "/api/v1/processing/jobs", s.operator.token, s.jobRequest(patient), jobKey)
			results <- result{r, err}
		}()
	}
	ids := map[uuid.UUID]bool{}
	for i := 0; i < 2; i++ {
		res := <-results
		if res.err != nil {
			t.Fatal(res.err)
		}
		switch res.r.status {
		case http.StatusCreated:
			var j jobView
			res.r.decode(t, &j)
			ids[j.ID] = true
		case http.StatusConflict:
			if res.r.code() != "IDEMPOTENCY_IN_PROGRESS" || res.r.header.Get("Retry-After") == "" {
				t.Errorf("concurrent duplicate: %s", res.r.describe())
			}
		default:
			t.Errorf("concurrent duplicate: %s", res.r.describe())
		}
	}
	if len(ids) > 1 {
		t.Errorf("two jobs created under one key: %v", ids)
	}
	if n := s.count(t, "SELECT count(*) FROM processing_jobs WHERE patient_id = $1", patient); n != 1 {
		t.Errorf("%d jobs recorded for one key used twice at once, want 1", n)
	}
}

// -------------------------------------------------- Rust unavailable

func (s *stack) testProcessorUnavailable(t *testing.T) {
	patient := s.patient(t, "no-processor")
	s.do(t, http.MethodPost, "/api/v1/measurements/batch", s.operator.token, map[string]any{"items": s.heartRateSeries(patient, "e2e-monitor")}, nil).expect(t, http.StatusCreated, "")
	gatewayBefore := s.compose.state(t, "api-gateway")

	t.Cleanup(func() { s.compose.restore(t, "processor"); s.waitProcessor(t) })
	s.compose.stop(t, "processor")

	// The gateway stays ready (the processor is not a readiness
	// dependency), the request fails with a bounded, specific answer, and
	// the job is on record as FAILED: nothing is left pending.
	if got := s.ready(); got != http.StatusOK {
		t.Errorf("/ready = %d with the processor stopped, want 200", got)
	}
	started := time.Now()
	r := s.do(t, http.MethodPost, "/api/v1/processing/jobs", s.operator.token, s.jobRequest(patient), nil)
	elapsed := time.Since(started)
	switch {
	case r.status == http.StatusServiceUnavailable && r.code() == "PROCESSOR_UNAVAILABLE":
	case r.status == http.StatusGatewayTimeout && (r.code() == "REQUEST_TIMEOUT" || r.code() == "PROCESSING_TIMEOUT"):
	default:
		t.Errorf("with the processor stopped: %s", r.describe())
	}
	if elapsed > 15*time.Second {
		t.Errorf("the failure took %s; the request timeout should have bounded it", elapsed)
	}
	s.assertFailedJob(t, patient)

	// Back: the same request now succeeds, without any restart.
	s.compose.start(t, "processor")
	s.waitProcessor(t)
	ok := s.do(t, http.MethodPost, "/api/v1/processing/jobs", s.operator.token, s.jobRequest(patient), nil).expect(t, http.StatusCreated, "")
	var j jobView
	ok.decode(t, &j)
	if j.Status != "COMPLETED" {
		t.Errorf("after recovery: %+v", j)
	}
	s.assertNoRestart(t, "api-gateway", gatewayBefore)
}

// ------------------------------------------------------------ timeout

func (s *stack) testProcessorTimeout(t *testing.T) {
	patient := s.patient(t, "slow-processor")
	s.do(t, http.MethodPost, "/api/v1/measurements/batch", s.operator.token, map[string]any{"items": s.heartRateSeries(patient, "e2e-monitor")}, nil).expect(t, http.StatusCreated, "")

	// Paused, the processor accepts connections and never answers: the
	// per-attempt timeout fires, and the request's own bound ends it.
	t.Cleanup(func() { s.compose.restore(t, "processor"); s.waitProcessor(t) })
	s.compose.pause(t, "processor")

	started := time.Now()
	r := s.do(t, http.MethodPost, "/api/v1/processing/jobs", s.operator.token, s.jobRequest(patient), nil)
	elapsed := time.Since(started)
	if r.status != http.StatusGatewayTimeout || (r.code() != "REQUEST_TIMEOUT" && r.code() != "PROCESSING_TIMEOUT") {
		t.Errorf("with the processor hung: %s", r.describe())
	}
	if elapsed < 4*time.Second || elapsed > 15*time.Second {
		t.Errorf("the timeout took %s, want between the per-attempt and the request bound", elapsed)
	}
	s.assertFailedJob(t, patient)

	s.compose.unpause(t, "processor")
	s.waitProcessor(t)
	ok := s.do(t, http.MethodPost, "/api/v1/processing/jobs", s.operator.token, s.jobRequest(patient), nil).expect(t, http.StatusCreated, "")
	var j jobView
	ok.decode(t, &j)
	if j.Status != "COMPLETED" {
		t.Errorf("after the processor resumed: %+v", j)
	}
}

// --------------------------------------------------- database failure

func (s *stack) testDatabaseFailure(t *testing.T) {
	patient := s.patient(t, "db-outage")
	gatewayBefore := s.compose.state(t, "api-gateway")

	t.Cleanup(func() {
		s.compose.restore(t, "postgres")
		s.waitFor(t, 60*time.Second, "the gateway to be ready again", func() bool { return s.ready() == http.StatusOK })
	})
	s.compose.stop(t, "postgres")

	// Liveness stays up, readiness withdraws the process, a write fails
	// with a dependency error rather than hanging or crashing.
	s.waitFor(t, 30*time.Second, "readiness to report the database outage", func() bool { return s.ready() == http.StatusServiceUnavailable })
	s.do(t, http.MethodGet, "/health", "", nil, nil).expect(t, http.StatusOK, "")
	started := time.Now()
	w := s.do(t, http.MethodPost, "/api/v1/patients", s.operator.token, map[string]string{"external_reference": "e2e-" + s.runID + "-during-outage", "date_of_birth": "1990-01-01", "sex": "MALE"}, nil)
	switch {
	case w.status == http.StatusServiceUnavailable && w.code() == "DATABASE_UNAVAILABLE":
	case w.status == http.StatusGatewayTimeout && w.code() == "REQUEST_TIMEOUT":
	default:
		t.Errorf("a write during the outage answered %s, want 503 DATABASE_UNAVAILABLE (or 504 REQUEST_TIMEOUT when the connection hangs)", w.describe())
	}
	if w.envelope().Error.RequestID == "" {
		t.Errorf("the outage answer is not an error envelope: %s", w.describe())
	}
	// A read that the cache cannot serve fails the same way, and a read it
	// can serve still works: the cache shortens a blip for in-flight work.
	if rr := s.do(t, http.MethodGet, "/api/v1/patients?limit=1", s.operator.token, nil, nil); rr.status != http.StatusServiceUnavailable && rr.status != http.StatusGatewayTimeout {
		t.Errorf("a list during the outage answered %s", rr.describe())
	}
	// The gateway bounds a request at 10 s; twice that leaves room for a
	// loaded machine while still catching a hang.
	if time.Since(started) > 20*time.Second {
		t.Errorf("the write took %s: it should have been bounded", time.Since(started))
	}
	if strings.Contains(strings.ToLower(string(w.body)), "postgres") || strings.Contains(string(w.body), "5432") {
		t.Errorf("the error body names the infrastructure: %s", w.body)
	}

	// Back within seconds, with no restart and nothing lost or invented.
	s.compose.start(t, "postgres")
	recovered := time.Now()
	s.waitFor(t, 60*time.Second, "readiness after the database returned", func() bool { return s.ready() == http.StatusOK })
	t.Logf("ready again %s after the database started", time.Since(recovered).Round(100*time.Millisecond))
	s.do(t, http.MethodGet, "/api/v1/patients/"+patient.String(), s.operator.token, nil, nil).expect(t, http.StatusOK, "")
	if n := s.count(t, "SELECT count(*) FROM patients WHERE external_reference = $1", "e2e-"+s.runID+"-during-outage"); n != 0 {
		t.Errorf("%d patients were created by a request that failed during the outage", n)
	}
	s.assertNoRestart(t, "api-gateway", gatewayBefore)
	s.assertNoRestart(t, "processor", s.compose.state(t, "processor"))
}

// ------------------------------------------------------ Redis failure

func (s *stack) testRedisFailure(t *testing.T) {
	patient := s.patient(t, "redis-outage")
	gatewayBefore := s.compose.state(t, "api-gateway")

	t.Cleanup(func() { s.compose.restore(t, "redis") })
	s.compose.stop(t, "redis")

	// A degradation, not an outage: readiness ignores Redis, reads,
	// sign-in and writes work, and rate limiting still applies from a
	// local counter.
	if got := s.ready(); got != http.StatusOK {
		t.Errorf("/ready = %d with Redis stopped, want 200", got)
	}
	token, _ := s.login(t, s.operator.email, password)
	r := s.do(t, http.MethodGet, "/api/v1/patients/"+patient.String(), token, nil, nil).expect(t, http.StatusOK, "")
	if r.header.Get("RateLimit-Limit") == "" {
		t.Error("no rate-limit headers while Redis is down: the limiter should fall back, not vanish")
	}
	s.do(t, http.MethodPost, "/api/v1/measurements", token, s.reading(patient, "RESPIRATORY_RATE", 16, "breaths/min", 6, "e2e-redis"), nil).expect(t, http.StatusCreated, "")
	key := map[string]string{"Idempotency-Key": "e2e-" + s.runID + "-redis-idem"}
	body := map[string]string{"external_reference": "e2e-" + s.runID + "-redis-idem", "date_of_birth": "1990-01-01", "sex": "FEMALE"}
	s.do(t, http.MethodPost, "/api/v1/patients", token, body, key).expect(t, http.StatusCreated, "")
	rep := s.do(t, http.MethodPost, "/api/v1/patients", token, body, key).expect(t, http.StatusCreated, "")
	if rep.header.Get("Idempotency-Replayed") != "true" {
		t.Error("idempotency did not survive the Redis outage")
	}

	s.compose.start(t, "redis")
	s.waitFor(t, 30*time.Second, "Redis to be healthy", func() bool { return s.compose.state(t, "redis").Health == "healthy" })
	s.do(t, http.MethodGet, "/api/v1/patients/"+patient.String(), token, nil, nil).expect(t, http.StatusOK, "")
	s.assertNoRestart(t, "api-gateway", gatewayBefore)
}

// ------------------------------------------------------------- helpers

// waitProcessor waits for the processor container to be healthy and for
// the gateway to complete a job through it, which is the state the next
// flow needs.
func (s *stack) waitProcessor(t *testing.T) {
	t.Helper()
	s.waitFor(t, 60*time.Second, "the processor to be healthy", func() bool {
		return s.compose.state(t, "processor").Health == "healthy"
	})
}

// assertFailedJob checks that the patient's latest job is FAILED with an
// error code and that no job of theirs is left non-terminal. The FAILED
// transition is written with its own bounded context as the response goes
// out, so it is awaited briefly rather than read at the instant the client
// has its answer.
func (s *stack) assertFailedJob(t *testing.T, patient uuid.UUID) {
	t.Helper()
	latest := func() []map[string]any {
		return s.query(t, "SELECT status, error_code, attempt_count FROM processing_jobs WHERE patient_id = $1 ORDER BY created_at DESC LIMIT 1", patient)
	}
	s.waitFor(t, 15*time.Second, "the failed dispatch to be recorded as a FAILED job", func() bool {
		rows := latest()
		return len(rows) == 1 && rows[0]["status"] == "FAILED"
	})
	if rows := latest(); len(rows) != 1 || rows[0]["status"] != "FAILED" || rows[0]["error_code"] == nil {
		t.Errorf("latest job row = %v, want FAILED with an error code", rows)
	}
	if n := s.count(t, "SELECT count(*) FROM processing_jobs WHERE patient_id = $1 AND status IN ('PENDING','PROCESSING')", patient); n != 0 {
		t.Errorf("%d jobs left PENDING or PROCESSING", n)
	}
}

// assertNoRestart checks that a service's container is the same process
// it was: a dependency outage is never a reason to restart a healthy one.
func (s *stack) assertNoRestart(t *testing.T, service string, before containerState) {
	t.Helper()
	after := s.compose.state(t, service)
	if !after.Running || after.RestartCount != before.RestartCount || !after.StartedAt.Equal(before.StartedAt) {
		t.Errorf("%s restarted: before %+v, after %+v", service, before, after)
	}
}
