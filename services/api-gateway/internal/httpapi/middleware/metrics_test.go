package middleware

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/n0ah-n0wa/VitalMesh/services/api-gateway/internal/observability/metrics"
)

type captured struct {
	method, route string
	status        int
	duration      time.Duration
}

type recordingMetrics struct {
	metrics.Noop // the measurements this test does not care about
	got          []captured
}

func (r *recordingMetrics) HTTPRequest(method, route string, status int, d time.Duration) {
	r.got = append(r.got, captured{method, route, status, d})
}

func TestMetricsRecordsRouteStatusAndDuration(t *testing.T) {
	rec := &recordingMetrics{}
	mux := http.NewServeMux()
	mux.Handle("GET /things/{id}", RecordRoute(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(2 * time.Millisecond)
		w.WriteHeader(http.StatusTeapot)
	})))
	h := Chain(mux, Metrics(rec))

	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/things/42", nil))
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/nope", nil))

	if len(rec.got) != 2 {
		t.Fatalf("recorded %d requests, want 2", len(rec.got))
	}
	if c := rec.got[0]; c.method != "GET" || c.route != "GET /things/{id}" || c.status != http.StatusTeapot || c.duration < 2*time.Millisecond {
		t.Errorf("matched request = %+v", c)
	}
	if strings.Contains(rec.got[0].route, "42") {
		t.Error("route label carries a path parameter value")
	}
	if c := rec.got[1]; c.route != UnmatchedRoute || c.status != http.StatusNotFound {
		t.Errorf("unmatched request = %+v", c)
	}
}

func TestMetricsAndLoggingShareTheRouteHolder(t *testing.T) {
	rec := &recordingMetrics{}
	logger, buf := testLogger()
	mux := http.NewServeMux()
	mux.Handle("POST /items", RecordRoute(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusCreated)
	})))
	h := Chain(mux, RequestID(), Logging(logger), Metrics(rec))
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/items", nil))

	if got := lastLogLine(t, buf)["route"]; got != "POST /items" {
		t.Errorf("logged route = %v", got)
	}
	if len(rec.got) != 1 || rec.got[0].route != "POST /items" || rec.got[0].status != http.StatusCreated {
		t.Errorf("metrics = %+v", rec.got)
	}
}

func TestMetricsNilRecorderIsSafe(t *testing.T) {
	h := Chain(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusNoContent) }), Metrics(nil))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if rec.Code != http.StatusNoContent {
		t.Errorf("status = %d", rec.Code)
	}
}
