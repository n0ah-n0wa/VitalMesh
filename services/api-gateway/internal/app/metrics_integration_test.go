//go:build integration

package app

import (
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/n0ah-n0wa/VitalMesh/services/api-gateway/internal/infra/redisclient/redistest"
	"github.com/n0ah-n0wa/VitalMesh/services/api-gateway/internal/observability/metrics"
)

// What the running gateway actually exposes at /metrics, driven by real
// requests against a real database and a real Redis. The unit tests in
// internal/observability/metrics prove the recorder; these prove the
// recorder is wired into the paths a request takes.

// The router names a route by its method and pattern together, which is
// also what the request log calls it, so that is the label these tests
// expect. The pattern is what keeps the label bounded; the method beside it
// is redundant but free, since it is determined by the pattern.

// scrapeGateway reads the exposition through the gateway's own router, so
// the route, the handler and the registry are all under test.
func scrapeGateway(t *testing.T, h *redisHarness) string {
	t.Helper()
	rec := h.do(http.MethodGet, "/metrics", "", "", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /metrics = %d: %s", rec.Code, rec.Body.String())
	}
	return rec.Body.String()
}

// sampleValue returns the value of one series, or -1 when it is absent.
func sampleValue(t *testing.T, body, series string) float64 {
	t.Helper()
	for _, line := range strings.Split(body, "\n") {
		// The value is the last field. Splitting on the first space would be
		// wrong: a label value may contain one, and the route label does,
		// because a route is named by its method and its pattern together.
		cut := strings.LastIndex(line, " ")
		if cut < 0 || line[:cut] != series {
			continue
		}
		parsed, err := strconv.ParseFloat(strings.TrimSpace(line[cut+1:]), 64)
		if err != nil {
			t.Fatalf("%s has an unparseable value %q", series, line[cut+1:])
		}
		return parsed
	}
	return -1
}

// The endpoint the specification names, served where the platform expects
// it: unversioned, next to the health endpoints.
func TestTheMetricsEndpointIsServedInTheExpositionFormat(t *testing.T) {
	h := newRedisHarness(t, redistest.URL(t), nil)

	rec := h.do(http.MethodGet, "/metrics", "", "", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /metrics = %d", rec.Code)
	}
	if got := rec.Header().Get("Content-Type"); !strings.Contains(got, "text/plain") {
		t.Errorf("Content-Type = %q, want the Prometheus text format", got)
	}
	if !strings.Contains(rec.Body.String(), "# HELP vitalmesh_") {
		t.Errorf("the scrape carries no application metrics:\n%s", rec.Body.String())
	}
}

// A request through the whole gateway must move the HTTP counters, and the
// route label must be the pattern rather than the path that was called.
func TestServingARequestMovesTheHTTPMetrics(t *testing.T) {
	h := newRedisHarness(t, redistest.URL(t), nil)
	path := "/api/v1/patients/" + h.patientID.String()

	before := sampleValue(t, scrapeGateway(t, h),
		`vitalmesh_http_requests_total{method="GET",route="GET /api/v1/patients/{patient_id}",status="200"}`)

	for i := 0; i < 3; i++ {
		if rec := h.do(http.MethodGet, path, "", h.token, nil); rec.Code != http.StatusOK {
			t.Fatalf("read %d = %d", i, rec.Code)
		}
	}

	body := scrapeGateway(t, h)
	after := sampleValue(t, body,
		`vitalmesh_http_requests_total{method="GET",route="GET /api/v1/patients/{patient_id}",status="200"}`)
	if after-max(before, 0) != 3 {
		t.Errorf("the counter moved by %v, want 3", after-max(before, 0))
	}
	// Latency is observed for the same route.
	if got := sampleValue(t, body,
		`vitalmesh_http_request_duration_seconds_count{method="GET",route="GET /api/v1/patients/{patient_id}"}`); got < 3 {
		t.Errorf("latency observations = %v, want at least 3", got)
	}
}

// A failed request has to be visible as an error, classified by whose fault
// it is, or an error rate cannot be alerted on.
func TestAFailedRequestIsCountedAsAnError(t *testing.T) {
	h := newRedisHarness(t, redistest.URL(t), nil)

	unknown := "/api/v1/patients/" + uuid.NewString()
	if rec := h.do(http.MethodGet, unknown, "", h.token, nil); rec.Code != http.StatusNotFound {
		t.Fatalf("unknown patient = %d, want 404", rec.Code)
	}

	body := scrapeGateway(t, h)
	if got := sampleValue(t, body,
		`vitalmesh_http_errors_total{class="client",method="GET",route="GET /api/v1/patients/{patient_id}"}`); got < 1 {
		t.Errorf("client errors = %v, want at least 1", got)
	}
}

// Every read touches PostgreSQL, so the database histogram must have moved.
func TestDatabaseLatencyIsRecordedForRealStatements(t *testing.T) {
	h := newRedisHarness(t, redistest.URL(t), nil)
	h.do(http.MethodGet, "/api/v1/patients/"+h.patientID.String(), "", h.token, nil)

	body := scrapeGateway(t, h)
	if got := sampleValue(t, body,
		`vitalmesh_database_query_duration_seconds_count{outcome="ok",statement="select"}`); got < 1 {
		t.Errorf("select observations = %v, want at least 1", got)
	}
	// The pool's saturation is published alongside, which is what separates
	// a slow database from one the gateway cannot reach.
	for _, series := range []string{
		"vitalmesh_database_connections_acquired",
		"vitalmesh_database_connections_idle",
		"vitalmesh_database_connections_max",
	} {
		if sampleValue(t, body, series) < 0 {
			t.Errorf("%s is not exposed", series)
		}
	}
}

// Redis is on the path of every rate-limit decision, so both its latency
// and the decision must be recorded.
func TestRedisLatencyAndRateLimitDecisionsAreRecorded(t *testing.T) {
	h := newRedisHarness(t, redistest.URL(t), map[string]string{
		"RATE_LIMIT_AUTHENTICATED": "2",
		"REDIS_NAMESPACE":          "metrics-" + uuid.NewString()[:8],
	})
	path := "/api/v1/patients/" + h.patientID.String()

	for i := 0; i < 3; i++ {
		h.do(http.MethodGet, path, "", h.token, nil)
	}

	body := scrapeGateway(t, h)
	if got := sampleValue(t, body,
		`vitalmesh_redis_command_duration_seconds_count{command="eval",outcome="ok"}`); got < 1 {
		t.Errorf("Redis eval observations = %v, want at least 1", got)
	}
	if got := sampleValue(t, body,
		`vitalmesh_rate_limit_decisions_total{backend="shared",decision="allowed"}`); got < 2 {
		t.Errorf("allowed decisions = %v, want at least 2", got)
	}
	if got := sampleValue(t, body,
		`vitalmesh_rate_limit_decisions_total{backend="shared",decision="limited"}`); got < 1 {
		t.Errorf("refusals = %v, want at least 1 after exceeding a limit of 2", got)
	}
}

// The gauge exists from start-up, so a dashboard shows "no jobs running"
// rather than nothing at all.
func TestActiveJobsIsPublishedAtZeroBeforeAnyJob(t *testing.T) {
	h := newRedisHarness(t, redistest.URL(t), nil)
	if got := sampleValue(t, scrapeGateway(t, h),
		`vitalmesh_operation_in_flight{operation="processing.job"}`); got != 0 {
		t.Errorf("active jobs = %v, want 0 published before any job", got)
	}
}

// ------------------------------------------------- bounded cardinality

// The guarantee that matters: no identifier may appear in the exposition
// as a label value, however many distinct resources are touched.
func TestNoIdentifierAppearsInTheExposition(t *testing.T) {
	h := newRedisHarness(t, redistest.URL(t), nil)

	// Touch several distinct resources, each with an id of its own.
	unknown := make([]string, 3)
	for i := range unknown {
		unknown[i] = uuid.NewString()
		h.do(http.MethodGet, "/api/v1/patients/"+unknown[i], "", h.token, nil)
	}
	h.do(http.MethodGet, "/api/v1/patients/"+h.patientID.String(), "", h.token, nil)

	body := scrapeGateway(t, h)
	for _, id := range append(unknown, h.patientID.String()) {
		if strings.Contains(body, id) {
			t.Errorf("an identifier reached the exposition: %s", id)
		}
	}

	// And nothing that merely looks like one, whatever its source.
	uuidLike := regexp.MustCompile(`[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}`)
	for _, line := range strings.Split(body, "\n") {
		if strings.HasPrefix(line, "vitalmesh_") && uuidLike.MatchString(line) {
			t.Errorf("a series carries an identifier:\n%s", line)
		}
	}
}

// Four requests to four different patients must produce one series, not
// four, or the metric grows with the data.
func TestDistinctResourcesShareOneSeries(t *testing.T) {
	h := newRedisHarness(t, redistest.URL(t), nil)

	for i := 0; i < 5; i++ {
		h.do(http.MethodGet, "/api/v1/patients/"+uuid.NewString(), "", h.token, nil)
	}

	series := 0
	for _, line := range strings.Split(scrapeGateway(t, h), "\n") {
		if strings.HasPrefix(line, "vitalmesh_http_requests_total{") &&
			strings.Contains(line, `route="GET /api/v1/patients/{patient_id}"`) {
			series++
		}
	}
	if series != 1 {
		t.Errorf("%d series for five distinct patients, want 1", series)
	}
}

// The recorder the gateway runs with is the Prometheus one, not the Noop
// that development used before this phase.
func TestTheGatewayRunsWithARealRecorder(t *testing.T) {
	h := newRedisHarness(t, redistest.URL(t), nil)
	body := scrapeGateway(t, h)
	if !strings.Contains(body, metrics.Namespace+"_http_requests_total") &&
		!strings.Contains(body, "# HELP "+metrics.Namespace+"_operation_in_flight") {
		t.Errorf("the gateway is not exporting application metrics:\n%s", body)
	}
}
