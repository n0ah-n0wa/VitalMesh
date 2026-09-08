package middleware

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/n0ah-n0wa/VitalMesh/services/api-gateway/internal/observability/tracing"
)

// spanLog records what a tracer was asked to start, so a test can assert
// on which requests produced a span and what it was named.
type spanLog struct {
	names      []string
	attributes map[string]any
}

func (l *spanLog) Start(ctx context.Context, name string) (context.Context, tracing.Span) {
	l.names = append(l.names, name)
	return ctx, loggedSpan{log: l}
}

type loggedSpan struct{ log *spanLog }

func (s loggedSpan) SetAttribute(key string, value any) {
	if s.log.attributes == nil {
		s.log.attributes = map[string]any{}
	}
	s.log.attributes[key] = value
}
func (s loggedSpan) SetName(name string) { s.log.names[len(s.log.names)-1] = name }
func (s loggedSpan) RecordError(error)   {}
func (s loggedSpan) End()                {}

func traced(tracer tracing.Tracer) http.Handler {
	return Chain(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}), Trace(tracer))
}

// A liveness probe every few seconds per replica would outnumber real
// traffic in any trace store. Probes are visible in metrics and logs, and
// deliberately absent from traces.
func TestProbesAreNotTraced(t *testing.T) {
	log := &spanLog{}
	handler := traced(log)

	for _, path := range []string{"/health", "/ready", "/metrics"} {
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code != http.StatusOK {
			t.Errorf("%s = %d: skipping the span must not change the response", path, rec.Code)
		}
	}
	if len(log.names) != 0 {
		t.Errorf("probes produced spans: %v", log.names)
	}

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/patients", nil))
	if len(log.names) != 1 {
		t.Fatalf("a real request produced %d spans, want 1", len(log.names))
	}
}

// The span is named by method and route, and the method is bounded the same
// way the metrics bound it: a client-chosen method must not name a span.
func TestASpanIsNamedByABoundedMethodAndTheRoute(t *testing.T) {
	log := &spanLog{}
	handler := traced(log)

	handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("FOO", "/api/v1/patients", nil))

	if len(log.names) != 1 {
		t.Fatalf("%d spans, want 1", len(log.names))
	}
	if got := log.names[0]; got != "OTHER unmatched" {
		t.Errorf("span name = %q, want the bounded method and the route", got)
	}
	if got := log.attributes["http.request.method"]; got != "OTHER" {
		t.Errorf("method attribute = %v, want OTHER", got)
	}
	if got := log.attributes["http.response.status_code"]; got != http.StatusOK {
		t.Errorf("status attribute = %v", got)
	}
}
