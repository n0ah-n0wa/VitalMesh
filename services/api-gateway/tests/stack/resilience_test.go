//go:build stack

package stack

import (
	"net/http"
	"strings"
	"testing"
	"time"
)

// leaseSweepBound is how long the suite allows the lease sweep to fail an
// interrupted job: scripts/stack-test.sh runs the stack with a 30 s lease
// and a 5 s sweep interval, so the job is failed within about 35 s of its
// dispatch; the rest is margin.
const leaseSweepBound = 90 * time.Second

// The failure-injection cases (SPECIFICATIONS.md sections 37, 52 and 90 to
// 94), beyond the dependency outages in failure_test.go. Each takes a real
// dependency away, or restarts a container, or saturates a resource, and
// asserts the four properties the specification requires of every failure:
// the request fails predictably (a documented status and code, bounded in
// time), nothing cascades (readiness and unrelated paths keep working, no
// container restarts that should not), retries are bounded (never
// uncontrolled, never a duplicate write), and the database and the job
// state machine stay correct (one terminal job per dispatch, no row lost
// or invented). docs/FAILURE_MODES.md is the prose; this is the check.

// testTransientProcessorRecovery is a slow downstream that recovers inside
// the request (section 37: transient failures, slow downstream responses).
// The processor is frozen when the job arrives and thawed a moment later,
// before the per-attempt timeout: the gateway waits, the processor answers,
// the job COMPLETES. No client retry, no failure recorded.
func (s *stack) testTransientProcessorRecovery(t *testing.T) {
	patient := s.patient(t, "transient")
	s.do(t, http.MethodPost, "/api/v1/measurements/batch", s.operator.token, map[string]any{"items": s.heartRateSeries(patient, "e2e-monitor")}, nil).expect(t, http.StatusCreated, "")
	before := s.compose.state(t, "api-gateway")

	t.Cleanup(func() { s.compose.restore(t, "processor"); s.waitProcessor(t) })
	s.compose.pause(t, "processor")

	// Thaw the processor after 1.5 s, well inside the 5 s per-attempt bound,
	// from another goroutine while the create call is in flight.
	done := make(chan struct{})
	go func() {
		time.Sleep(1500 * time.Millisecond)
		s.compose.unpause(t, "processor")
		close(done)
	}()

	started := time.Now()
	r := s.do(t, http.MethodPost, "/api/v1/processing/jobs", s.operator.token, s.jobRequest(patient), nil)
	<-done
	elapsed := time.Since(started)
	r.expect(t, http.StatusCreated, "")
	var j jobView
	r.decode(t, &j)
	if j.Status != "COMPLETED" {
		t.Errorf("a job over a stall that recovered = %s, want COMPLETED", j.Status)
	}
	if elapsed > 10*time.Second {
		t.Errorf("the recovered request took %s; a stall shorter than the attempt bound should not have retried", elapsed)
	}
	// Exactly one job, and it holds results: the stall left no debris.
	if n := s.count(t, "SELECT count(*) FROM processing_jobs WHERE patient_id = $1", patient); n != 1 {
		t.Errorf("%d jobs for one dispatch over a stall, want 1", n)
	}
	if n := s.count(t, "SELECT count(*) FROM processing_results WHERE job_id = $1", j.ID); n == 0 {
		t.Error("the completed job has no results")
	}
	s.assertNoRestart(t, "api-gateway", before)
	s.assertNoRestart(t, "processor", s.compose.state(t, "processor"))
}

// testBoundedRetries proves the gateway's retries are bounded and leave no
// duplicate (section 37: retries must never create uncontrolled duplicate
// writes; section 93). The processor stays frozen, so every attempt times
// out; the job FAILS, its attempt_count is within the configured maximum,
// the gateway logged no more "may retry" lines than that for the request,
// and exactly one job row exists.
func (s *stack) testBoundedRetries(t *testing.T) {
	const maxAttempts = 3 // PROCESSOR_MAX_ATTEMPTS default; the ceiling, not the target
	patient := s.patient(t, "bounded-retries")
	s.do(t, http.MethodPost, "/api/v1/measurements/batch", s.operator.token, map[string]any{"items": s.heartRateSeries(patient, "e2e-monitor")}, nil).expect(t, http.StatusCreated, "")

	t.Cleanup(func() { s.compose.restore(t, "processor"); s.waitProcessor(t) })
	s.compose.pause(t, "processor")

	r := s.do(t, http.MethodPost, "/api/v1/processing/jobs", s.operator.token, s.jobRequest(patient), nil)
	if r.status != http.StatusGatewayTimeout && r.status != http.StatusServiceUnavailable {
		t.Fatalf("with the processor frozen: %s", r.describe())
	}
	request := r.requestID()
	s.assertFailedJob(t, patient)

	rows := s.query(t, "SELECT attempt_count FROM processing_jobs WHERE patient_id = $1 ORDER BY created_at DESC LIMIT 1", patient)
	attempts, _ := rows[0]["attempt_count"].(int32)
	if attempts < 1 || int(attempts) > maxAttempts {
		t.Errorf("attempt_count = %d, want between 1 and %d", attempts, maxAttempts)
	}
	// The gateway's own log is the second witness: one "may retry" line per
	// retry of this request, never more than the ceiling.
	logs := s.compose.must(t, "logs", "--no-color", "--no-log-prefix", "api-gateway")
	retries := 0
	for _, line := range strings.Split(logs, "\n") {
		if strings.Contains(line, request) && strings.Contains(line, "processor call failed, may retry") {
			retries++
		}
	}
	if retries > maxAttempts {
		t.Errorf("the gateway logged %d retry lines for request %s, want at most %d", retries, request, maxAttempts)
	}
	// One dispatch, one job: a retry never became a second job.
	if n := s.count(t, "SELECT count(*) FROM processing_jobs WHERE patient_id = $1", patient); n != 1 {
		t.Errorf("%d jobs for one dispatch, want 1: a retry created a duplicate", n)
	}
}

// testDatabaseConnectionsSevered is a transient network failure at the
// database: the gateway's pooled connections are killed from the server
// side (as a network reset or a failover would), and the pool must
// re-establish them without the gateway restarting and without losing or
// inventing data (sections 37 and 90).
func (s *stack) testDatabaseConnectionsSevered(t *testing.T) {
	patient := s.patient(t, "severed")
	before := s.compose.state(t, "api-gateway")
	// A read first, so the gateway holds at least one pooled connection to
	// sever.
	s.do(t, http.MethodGet, "/api/v1/patients/"+patient.String(), s.operator.token, nil, nil).expect(t, http.StatusOK, "")

	// Kill every client backend that is not the local psql session (the
	// gateway connects over TCP, so its connections have a client address;
	// the exec'd psql is over the unix socket and has none).
	killed := s.compose.exec(t, "postgres", "psql", "-U", "vitalmesh", "-d", "vitalmesh", "-Atc",
		"SELECT count(pg_terminate_backend(pid)) FROM pg_stat_activity WHERE usename='vitalmesh' AND datname='vitalmesh' AND backend_type='client backend' AND client_addr IS NOT NULL AND pid <> pg_backend_pid()")
	t.Logf("severed %s gateway database connection(s)", strings.TrimSpace(killed))

	// The gateway must recover on its own: the pool reconnects, and reads
	// serve again within seconds. Readiness may blip to 503 and must return.
	s.waitFor(t, 30*time.Second, "the gateway to serve reads again after its connections were severed", func() bool {
		r, err := s.raw(http.MethodGet, "/api/v1/patients/"+patient.String(), s.operator.token, nil, nil)
		return err == nil && r.status == http.StatusOK
	})
	s.waitFor(t, 30*time.Second, "readiness to recover", func() bool { return s.ready() == http.StatusOK })

	// Integrity: the patient is intact, and a write works, proving the
	// reconnected pool is fully usable.
	s.do(t, http.MethodPost, "/api/v1/measurements", s.operator.token, s.reading(patient, "SPO2", 97, "%", 7, "e2e-severed"), nil).expect(t, http.StatusCreated, "")
	s.assertNoRestart(t, "api-gateway", before)
}

// testProcessorRestart restarts the processor container (a pod restart of
// the downstream): a job during the restart fails to a terminal state, a
// job after it completes, and the gateway does not restart (sections 37,
// 52, 92).
func (s *stack) testProcessorRestart(t *testing.T) {
	patient := s.patient(t, "processor-restart")
	s.do(t, http.MethodPost, "/api/v1/measurements/batch", s.operator.token, map[string]any{"items": s.heartRateSeries(patient, "e2e-monitor")}, nil).expect(t, http.StatusCreated, "")
	before := s.compose.state(t, "api-gateway")

	// Stop it, submit while it is down (terminal FAILED), then start it.
	s.compose.stop(t, "processor")
	if got := s.ready(); got != http.StatusOK {
		t.Errorf("/ready = %d with the processor stopped, want 200 (it is not a readiness dependency)", got)
	}
	r := s.do(t, http.MethodPost, "/api/v1/processing/jobs", s.operator.token, s.jobRequest(patient), nil)
	if r.status < 500 {
		t.Errorf("a job with the processor stopped answered %s, want a 5xx", r.describe())
	}
	s.assertFailedJob(t, patient)

	s.compose.start(t, "processor")
	s.waitProcessor(t)
	ok := s.do(t, http.MethodPost, "/api/v1/processing/jobs", s.operator.token, s.jobRequest(patient), nil).expect(t, http.StatusCreated, "")
	var j jobView
	ok.decode(t, &j)
	if j.Status != "COMPLETED" {
		t.Errorf("a job after the processor returned = %s, want COMPLETED", j.Status)
	}
	after := s.compose.state(t, "processor")
	if after.RestartCount != before.RestartCount {
		// The processor itself was restarted on purpose; only assert it is
		// healthy again, which waitProcessor already did.
		_ = after
	}
	s.assertNoRestart(t, "api-gateway", before)
}

// testGatewayRestart restarts the gateway container itself (a pod restart
// of the service): its data is unchanged, a write after it works, and no
// job is left non-terminal, so a restart during operation neither loses
// nor duplicates state (sections 37, 38, 92).
func (s *stack) testGatewayRestart(t *testing.T) {
	patient := s.patient(t, "gateway-restart")
	s.do(t, http.MethodPost, "/api/v1/measurements/batch", s.operator.token, map[string]any{"items": s.heartRateSeries(patient, "e2e-monitor")}, nil).expect(t, http.StatusCreated, "")
	job := s.do(t, http.MethodPost, "/api/v1/processing/jobs", s.operator.token, s.jobRequest(patient), nil).expect(t, http.StatusCreated, "")
	var j jobView
	job.decode(t, &j)

	patientsBefore := s.count(t, "SELECT count(*) FROM patients WHERE deleted_at IS NULL")
	readingsBefore := s.count(t, "SELECT count(*) FROM measurements")
	jobsBefore := s.count(t, "SELECT count(*) FROM processing_jobs")
	resultsBefore := s.count(t, "SELECT count(*) FROM processing_results")

	s.compose.restart(t, "api-gateway")
	s.waitFor(t, 60*time.Second, "the gateway to be ready after its restart", func() bool { return s.ready() == http.StatusOK })

	// Nothing lost, nothing invented across the restart.
	for _, c := range []struct {
		what   string
		before int64
		sql    string
	}{
		{"patients", patientsBefore, "SELECT count(*) FROM patients WHERE deleted_at IS NULL"},
		{"measurements", readingsBefore, "SELECT count(*) FROM measurements"},
		{"jobs", jobsBefore, "SELECT count(*) FROM processing_jobs"},
		{"results", resultsBefore, "SELECT count(*) FROM processing_results"},
	} {
		if after := s.count(t, c.sql); after != c.before {
			t.Errorf("%s: %d before the restart, %d after", c.what, c.before, after)
		}
	}
	if n := s.count(t, "SELECT count(*) FROM processing_jobs WHERE status IN ('PENDING','PROCESSING')"); n != 0 {
		t.Errorf("%d jobs left non-terminal across the restart", n)
	}
	// A fresh token (the process is new) and a write that works.
	token, _ := s.login(t, s.operator.email, password)
	s.do(t, http.MethodPost, "/api/v1/measurements", token, s.reading(patient, "SPO2", 96, "%", 8, "e2e-after-restart"), nil).expect(t, http.StatusCreated, "")
	// The completed job is still readable and still COMPLETED.
	back := s.do(t, http.MethodGet, "/api/v1/processing/jobs/"+j.ID.String(), token, nil, nil).expect(t, http.StatusOK, "")
	var read jobView
	back.decode(t, &read)
	if read.Status != "COMPLETED" {
		t.Errorf("the job created before the restart reads back as %s", read.Status)
	}
}

// testGatewayKilledMidJob kills the gateway with SIGKILL while a job is in
// flight: a crash, an OOM kill or a lost node, which no graceful shutdown
// can drain. The job is PROCESSING when the process dies and must not stay
// so for ever: once its lease expires the restarted gateway's sweep fails
// it with PROCESSING_INTERRUPTED, the state machine holds, no results are
// invented for it, and the next job completes (sections 37, 92, 94).
func (s *stack) testGatewayKilledMidJob(t *testing.T) {
	patient := s.patient(t, "killed-mid-job")
	s.do(t, http.MethodPost, "/api/v1/measurements/batch", s.operator.token, map[string]any{"items": s.heartRateSeries(patient, "e2e-monitor")}, nil).expect(t, http.StatusCreated, "")

	t.Cleanup(func() {
		s.compose.restore(t, "processor")
		s.waitProcessor(t)
		s.compose.restore(t, "api-gateway")
		s.waitFor(t, 60*time.Second, "the gateway to be ready after the killed-mid-job case", func() bool { return s.ready() == http.StatusOK })
	})

	// The processor is frozen, so the dispatch stays in flight until the
	// gateway is killed under it.
	s.compose.pause(t, "processor")
	inflight := make(chan struct{})
	go func() {
		defer close(inflight)
		_, _ = s.raw(http.MethodPost, "/api/v1/processing/jobs", s.operator.token, s.jobRequest(patient), nil)
	}()
	var jobID string
	s.waitFor(t, 15*time.Second, "the job to be PROCESSING before the gateway is killed", func() bool {
		rows := s.query(t, "SELECT id::text AS id, status FROM processing_jobs WHERE patient_id = $1", patient)
		if len(rows) != 1 || rows[0]["status"] != "PROCESSING" {
			return false
		}
		jobID, _ = rows[0]["id"].(string)
		return jobID != ""
	})

	s.compose.kill(t, "api-gateway")
	<-inflight
	s.compose.unpause(t, "processor")
	s.waitProcessor(t)
	// The restart policy brings the container back on its own; start is
	// what makes that explicit, and a no-op when it already has.
	s.compose.start(t, "api-gateway")
	s.waitFor(t, 60*time.Second, "the gateway to be ready after being killed", func() bool { return s.ready() == http.StatusOK })

	// Right after the restart the job is still PROCESSING: its lease has
	// not expired, and nothing guesses that a job in flight is dead.
	if rows := s.query(t, "SELECT status FROM processing_jobs WHERE id = $1::uuid", jobID); len(rows) != 1 || rows[0]["status"] != "PROCESSING" {
		t.Errorf("job right after the restart = %v, want still PROCESSING until its lease expires", rows)
	}
	// Then the lease expires and the sweep fails it, with the code that
	// says what happened.
	s.waitFor(t, leaseSweepBound, "the interrupted job to be failed by the lease sweep", func() bool {
		rows := s.query(t, "SELECT status FROM processing_jobs WHERE id = $1::uuid", jobID)
		return len(rows) == 1 && rows[0]["status"] == "FAILED"
	})
	rows := s.query(t, "SELECT status, error_code, error_message, attempt_count, failed_at IS NOT NULL AS has_failed_at FROM processing_jobs WHERE id = $1::uuid", jobID)
	if len(rows) != 1 {
		t.Fatalf("job %s: %d rows", jobID, len(rows))
	}
	if code, _ := rows[0]["error_code"].(string); code != "PROCESSING_INTERRUPTED" {
		t.Errorf("error_code = %v, want PROCESSING_INTERRUPTED", rows[0]["error_code"])
	}
	if msg, _ := rows[0]["error_message"].(string); msg == "" {
		t.Error("the interrupted job has no error message")
	}
	if attempts, _ := rows[0]["attempt_count"].(int32); attempts != 1 {
		t.Errorf("attempt_count = %d, want 1: the interrupted attempt was real and no other was made", attempts)
	}
	if has, _ := rows[0]["has_failed_at"].(bool); !has {
		t.Error("the interrupted job has no failed_at")
	}
	if n := s.count(t, "SELECT count(*) FROM processing_results WHERE job_id = $1::uuid", jobID); n != 0 {
		t.Errorf("%d results for a job that never finished, want 0", n)
	}
	if n := s.count(t, "SELECT count(*) FROM processing_jobs WHERE patient_id = $1 AND status IN ('PENDING','PROCESSING')", patient); n != 0 {
		t.Errorf("%d jobs left non-terminal", n)
	}

	// The gateway is whole: a new job for the same patient completes.
	ok := s.do(t, http.MethodPost, "/api/v1/processing/jobs", s.operator.token, s.jobRequest(patient), nil).expect(t, http.StatusCreated, "")
	var j jobView
	ok.decode(t, &j)
	if j.Status != "COMPLETED" {
		t.Errorf("a job after the interrupted one = %s, want COMPLETED", j.Status)
	}
}
