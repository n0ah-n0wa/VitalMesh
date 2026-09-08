package metrics

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
)

// scrape returns the exposition text, which is what a Prometheus server
// actually sees. Asserting on it rather than on the recorder's fields is
// what makes these tests about the metrics rather than about the struct.
func scrape(t *testing.T, p *Prometheus) string {
	t.Helper()
	rec := httptest.NewRecorder()
	p.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("scrape = %d: %s", rec.Code, rec.Body.String())
	}
	return rec.Body.String()
}

// ------------------------------------------------------- registration

// The families whose labels are a small closed set are published at zero, so
// an alert on them works from the first scrape rather than from the first
// occurrence.
func TestClosedFamiliesArePublishedBeforeAnyTraffic(t *testing.T) {
	body := scrape(t, NewPrometheus())
	for _, series := range []string{
		`vitalmesh_rate_limit_decisions_total{backend="shared",decision="limited"} 0`,
		`vitalmesh_rate_limit_decisions_total{backend="local",decision="allowed"} 0`,
		`vitalmesh_operation_in_flight{operation="processing.job"} 0`,
	} {
		if !strings.Contains(body, series) {
			t.Errorf("a scrape before any traffic is missing: %s", series)
		}
	}
}

// Everything the specification asks for has to exist, with help text, once
// the code that reports it has run once. This exercises each recorder
// method and then reads the exposition a Prometheus server would see.
func TestEveryRequiredMetricIsExposed(t *testing.T) {
	p := NewPrometheus()
	p.HTTPRequest(http.MethodGet, "/api/v1/patients", 200, time.Millisecond)
	p.HTTPRequest(http.MethodGet, "/api/v1/patients", 500, time.Millisecond)
	p.Operation("processing.create", OutcomeOK, time.Second)
	p.Batch("measurement.batch", 4)
	p.Cache("patient.get", true)
	p.Database(StatementSelect, OutcomeOK, time.Millisecond)
	p.Redis("get", OutcomeOK, time.Millisecond)
	body := scrape(t, p)

	required := map[string]string{
		"HTTP request count":   "vitalmesh_http_requests_total",
		"HTTP request latency": "vitalmesh_http_request_duration_seconds",
		"HTTP error count":     "vitalmesh_http_errors_total",
		"database latency":     "vitalmesh_database_query_duration_seconds",
		"Redis latency":        "vitalmesh_redis_command_duration_seconds",
		"processing jobs":      "vitalmesh_operation_total",
		"processing duration":  "vitalmesh_operation_duration_seconds",
		"active jobs":          "vitalmesh_operation_in_flight",
		"batch sizes":          "vitalmesh_batch_size",
		"rate-limit events":    "vitalmesh_rate_limit_decisions_total",
		"cache reads":          "vitalmesh_cache_reads_total",
	}
	for requirement, metric := range required {
		if !strings.Contains(body, "# TYPE "+metric) {
			t.Errorf("%s: %s is not registered", requirement, metric)
		}
		if !strings.Contains(body, "# HELP "+metric) {
			t.Errorf("%s: %s has no help text", requirement, metric)
		}
	}
}

// A registry that rejects a duplicate is what stops two components
// publishing the same series under different meanings.
func TestBuildingTwoRecordersDoesNotShareState(t *testing.T) {
	first, second := NewPrometheus(), NewPrometheus()
	first.HTTPRequest(http.MethodGet, "/api/v1/patients", 200, time.Millisecond)

	if got := testutil.ToFloat64(first.httpRequests.WithLabelValues("GET", "/api/v1/patients", "200")); got != 1 {
		t.Errorf("first recorder counted %v", got)
	}
	if got := testutil.CollectAndCount(second.httpRequests); got != 0 {
		t.Errorf("the second recorder shares the first's series: %d", got)
	}
}

// The process and runtime collectors answer "is the service healthy" when
// the application metrics cannot.
func TestTheRuntimeCollectorsAreRegistered(t *testing.T) {
	body := scrape(t, NewPrometheus())
	for _, metric := range []string{"go_goroutines", "go_memstats_alloc_bytes"} {
		if !strings.Contains(body, metric) {
			t.Errorf("%s is not exposed", metric)
		}
	}
}

// ------------------------------------------------------ instrumentation

func TestAServedRequestIsCountedTimedAndClassified(t *testing.T) {
	p := NewPrometheus()
	p.HTTPRequest(http.MethodGet, "/api/v1/patients/{patient_id}", 200, 30*time.Millisecond)
	p.HTTPRequest(http.MethodPost, "/api/v1/patients", 422, 5*time.Millisecond)
	p.HTTPRequest(http.MethodPost, "/api/v1/patients", 500, 5*time.Millisecond)

	if got := testutil.ToFloat64(p.httpRequests.WithLabelValues("GET", "/api/v1/patients/{patient_id}", "200")); got != 1 {
		t.Errorf("requests = %v, want 1", got)
	}
	// An error count exists so that "is it us or them" is one query.
	if got := testutil.ToFloat64(p.httpErrors.WithLabelValues("POST", "/api/v1/patients", "client")); got != 1 {
		t.Errorf("client errors = %v, want 1", got)
	}
	if got := testutil.ToFloat64(p.httpErrors.WithLabelValues("POST", "/api/v1/patients", "server")); got != 1 {
		t.Errorf("server errors = %v, want 1", got)
	}
	// A success must not be counted as an error.
	if got := testutil.CollectAndCount(p.httpErrors); got != 2 {
		t.Errorf("%d error series, want exactly the two failures", got)
	}
	if got := testutil.CollectAndCount(p.httpLatency); got != 2 {
		t.Errorf("%d latency series, want one per method and route", got)
	}
}

func TestDependencyLatencyIsRecordedWithItsOutcome(t *testing.T) {
	p := NewPrometheus()
	p.Database(StatementSelect, OutcomeOK, 2*time.Millisecond)
	p.Database(StatementInsert, OutcomeError, time.Millisecond)
	p.Redis("get", OutcomeOK, time.Millisecond)
	p.Redis("eval", OutcomeUnavailable, 0)

	body := scrape(t, p)
	for _, want := range []string{
		`vitalmesh_database_query_duration_seconds_count{outcome="ok",statement="select"} 1`,
		`vitalmesh_database_query_duration_seconds_count{outcome="error",statement="insert"} 1`,
		`vitalmesh_redis_command_duration_seconds_count{command="get",outcome="ok"} 1`,
		`vitalmesh_redis_command_duration_seconds_count{command="eval",outcome="unavailable"} 1`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("missing series:\n%s", want)
		}
	}
}

func TestProcessingJobsDurationFailuresAndActiveCount(t *testing.T) {
	p := NewPrometheus()
	p.Operation("processing.create", OutcomeOK, 2*time.Second)
	p.Operation("processing.create", OutcomeError, time.Second)
	p.InFlight("processing.job", 1)
	p.InFlight("processing.job", 1)
	p.InFlight("processing.job", -1)
	p.Batch("processing.results", 12)

	if got := testutil.ToFloat64(p.operations.WithLabelValues("processing.create", "ok")); got != 1 {
		t.Errorf("jobs = %v", got)
	}
	// Failures are the same counter under another outcome, so a rate of
	// failures and a rate of jobs come from one family.
	if got := testutil.ToFloat64(p.operations.WithLabelValues("processing.create", "error")); got != 1 {
		t.Errorf("failures = %v", got)
	}
	if got := testutil.ToFloat64(p.inFlight.WithLabelValues("processing.job")); got != 1 {
		t.Errorf("active jobs = %v, want 1 after two starts and one finish", got)
	}
	if !strings.Contains(scrape(t, p), `vitalmesh_batch_size_count{operation="processing.results"} 1`) {
		t.Error("the batch size was not recorded")
	}
}

func TestRateLimitEventsAreCountedByDecisionAndBackend(t *testing.T) {
	p := NewPrometheus()
	p.RateLimit(DecisionAllowed, "shared")
	p.RateLimit(DecisionLimited, "shared")
	p.RateLimit(DecisionAllowed, "local")

	if got := testutil.ToFloat64(p.rateLimit.WithLabelValues(DecisionLimited, "shared")); got != 1 {
		t.Errorf("refusals = %v", got)
	}
	// Four series exist from construction, one per decision and backend;
	// recording moves them rather than creating more.
	if got := testutil.CollectAndCount(p.rateLimit); got != 4 {
		t.Errorf("%d rate-limit series, want the four the closed labels allow", got)
	}
}

func TestCacheReadsAreCountedByResult(t *testing.T) {
	p := NewPrometheus()
	p.Cache("patient.get", true)
	p.Cache("patient.get", false)

	body := scrape(t, p)
	for _, want := range []string{
		`vitalmesh_cache_reads_total{operation="patient.get",result="hit"} 1`,
		`vitalmesh_cache_reads_total{operation="patient.get",result="miss"} 1`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("missing series:\n%s", want)
		}
	}
}

// -------------------------------------------------- bounded cardinality

// The rule that matters most: an identifier must never become a label. The
// port has no parameter that could carry one, and this checks the shape of
// every label the recorder can emit.
func TestNoMetricTakesAnIdentifierAsALabel(t *testing.T) {
	p := NewPrometheus()
	// Exercise every method with the values a real request produces.
	p.HTTPRequest(http.MethodGet, "/api/v1/patients/{patient_id}", 200, time.Millisecond)
	p.Operation("patient.get", OutcomeOK, time.Millisecond)
	p.Batch("measurement.batch", 3)
	p.Cache("patient.get", true)
	p.Database(StatementSelect, OutcomeOK, time.Millisecond)
	p.Redis("get", OutcomeOK, time.Millisecond)
	p.RateLimit(DecisionAllowed, "shared")
	p.InFlight("processing.job", 1)

	allowed := map[string]map[string]bool{
		"method":    {"GET": true, "POST": true, "PUT": true, "PATCH": true, "DELETE": true},
		"class":     {"client": true, "server": true},
		"outcome":   {"ok": true, "invalid": true, "not_found": true, "conflict": true, "denied": true, "error": true, "cancelled": true, "unavailable": true},
		"result":    {"hit": true, "miss": true},
		"decision":  {DecisionAllowed: true, DecisionLimited: true},
		"backend":   {"shared": true, "local": true},
		"statement": {StatementSelect: true, StatementInsert: true, StatementUpdate: true, StatementDelete: true, StatementBegin: true, StatementCommit: true, StatementRollback: true, StatementOther: true},
	}

	families, err := p.Registry().Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	for _, family := range families {
		if !strings.HasPrefix(family.GetName(), Namespace+"_") {
			continue // runtime collectors are not ours to constrain
		}
		for _, metric := range family.GetMetric() {
			for _, label := range metric.GetLabel() {
				name, value := label.GetName(), label.GetValue()
				if values, closed := allowed[name]; closed && !values[value] {
					t.Errorf("%s: label %s=%q is outside its vocabulary", family.GetName(), name, value)
				}
				if looksLikeIdentifier(value) {
					t.Errorf("%s: label %s=%q looks like an identifier", family.GetName(), name, value)
				}
			}
		}
	}
}

// looksLikeIdentifier catches the shapes this application's identifiers
// take: a UUID, or a request id, which is 26 characters of Crockford
// base32.
func looksLikeIdentifier(value string) bool {
	if len(value) == 36 && strings.Count(value, "-") == 4 {
		return true
	}
	if len(value) != 26 {
		return false
	}
	for _, r := range value {
		if !strings.ContainsRune("0123456789ABCDEFGHJKMNPQRSTVWXYZ", r) {
			return false
		}
	}
	return true
}

// A route label is a registered pattern, so a path carrying an identifier
// can never widen the label space however many distinct paths are called.
func TestTheRouteLabelIsAPatternNotAPath(t *testing.T) {
	p := NewPrometheus()
	for i := 0; i < 100; i++ {
		p.HTTPRequest(http.MethodGet, "/api/v1/patients/{patient_id}", 200, time.Millisecond)
	}
	if got := testutil.CollectAndCount(p.httpRequests); got != 1 {
		t.Errorf("%d series after 100 requests to distinct patients, want 1", got)
	}
}

// ---------------------------------------------------- statement naming

func TestStatementReducesSQLToABoundedVerb(t *testing.T) {
	cases := map[string]string{
		"SELECT id FROM patients WHERE external_reference = $1": StatementSelect,
		"\n\tinsert into measurements (id) values ($1)":         StatementInsert,
		"UPDATE processing_jobs SET status = $2":                StatementUpdate,
		"DELETE FROM idempotency_keys WHERE id = $1":            StatementDelete,
		"begin":    StatementBegin,
		"commit":   StatementCommit,
		"rollback": StatementRollback,
		"WITH recent AS (SELECT 1) SELECT * FROM recent": StatementOther,
		"": StatementOther,
	}
	for sql, want := range cases {
		if got := Statement(sql); got != want {
			t.Errorf("Statement(%q) = %q, want %q", sql, got, want)
		}
	}
}

// The verb is all that is taken, so a value inside a statement cannot reach
// a label even when the statement embeds one.
func TestStatementNeverCarriesAValueFromTheSQL(t *testing.T) {
	const sql = "SELECT * FROM patients WHERE external_reference = 'MRN-88231'"
	if got := Statement(sql); strings.Contains(got, "MRN") || got != StatementSelect {
		t.Errorf("Statement = %q", got)
	}
}

// The method is the one label a client controls. HTTP permits any token as
// a method, so recording it verbatim would let a caller create a series per
// request; only routable methods are kept and the rest share one bucket.
func TestAnInventedMethodDoesNotCreateASeries(t *testing.T) {
	p := NewPrometheus()
	for _, method := range []string{"FOO", "BAR", "BAZ", "QUX", "ZAP", "ZIP"} {
		p.HTTPRequest(method, "unmatched", 405, time.Millisecond)
	}
	p.HTTPRequest(http.MethodGet, "GET /health", 200, time.Millisecond)

	if got := testutil.CollectAndCount(p.httpRequests); got != 2 {
		t.Errorf("%d series after six invented methods, want 2 (one bucket plus GET)", got)
	}
	if got := testutil.ToFloat64(p.httpRequests.WithLabelValues(MethodOther, "unmatched", "405")); got != 6 {
		t.Errorf("the OTHER bucket holds %v, want all 6", got)
	}
	for _, method := range []string{"GET", "HEAD", "POST", "PUT", "PATCH", "DELETE", "OPTIONS"} {
		if Method(method) != method {
			t.Errorf("Method(%q) = %q, want it kept: it is routable", method, Method(method))
		}
	}
	if Method("get") != MethodOther {
		t.Error("a lower-case method is not one the router routes and must not be kept")
	}
}
