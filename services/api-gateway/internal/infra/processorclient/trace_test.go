package processorclient

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/n0ah-n0wa/VitalMesh/services/api-gateway/internal/config"
	"github.com/n0ah-n0wa/VitalMesh/services/api-gateway/internal/observability/tracing"
)

// spanLog records the spans a tracer was asked for.
type spanLog struct {
	mu     sync.Mutex
	names  []string
	errors []string
	attrs  map[string]any
}

func (l *spanLog) Start(ctx context.Context, name string) (context.Context, tracing.Span) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.names = append(l.names, name)
	return ctx, loggedSpan{log: l}
}

type loggedSpan struct{ log *spanLog }

func (s loggedSpan) SetAttribute(key string, value any) {
	s.log.mu.Lock()
	defer s.log.mu.Unlock()
	if s.log.attrs == nil {
		s.log.attrs = map[string]any{}
	}
	s.log.attrs[key] = value
}
func (s loggedSpan) SetName(string) {}
func (s loggedSpan) RecordError(err error) {
	s.log.mu.Lock()
	defer s.log.mu.Unlock()
	s.log.errors = append(s.log.errors, err.Error())
}
func (s loggedSpan) End() {}

func tracedClient(t *testing.T, baseURL string, tracer tracing.Tracer, attempts int) *Client {
	t.Helper()
	return New(config.Processor{
		BaseURL:         baseURL,
		Token:           config.Secret("test-token-test-token"),
		Timeout:         2 * time.Second,
		MaxAttempts:     attempts,
		Backoff:         time.Millisecond,
		MaxBackoff:      2 * time.Millisecond,
		ContractVersion: config.InternalContractVersion,
	}, discardLogger(), Options{
		Tracer: tracer,
		Sleep:  func(context.Context, time.Duration) error { return nil },
	})
}

// A retried call must show in the trace as what it was: several attempts,
// each with its own outcome, rather than one long gap between the gateway's
// span and the processor's.
func TestEachAttemptOpensAClientSpanWithItsOutcome(t *testing.T) {
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls++
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(`{"error":{"code":"OVERLOADED","message":"busy","retryable":true}}`))
	}))
	defer srv.Close()

	log := &spanLog{}
	c := tracedClient(t, srv.URL, log, 3)
	_, err := c.Process(context.Background(), request())
	if err == nil {
		t.Fatal("a call the processor refused three times succeeded")
	}

	spans := 0
	for _, name := range log.names {
		if name == "processor.process" {
			spans++
		}
	}
	if spans != calls || spans != 3 {
		t.Errorf("%d attempts produced %d client spans, want one each (3)", calls, spans)
	}
	if len(log.errors) != 3 {
		t.Errorf("%d attempts recorded %d span errors, want one each", calls, len(log.errors))
	}
	if got := log.attrs["http.response.status_code"]; got != http.StatusServiceUnavailable {
		t.Errorf("status attribute = %v, want 503", got)
	}
	if got, _ := log.attrs["server.address"].(string); !strings.Contains(srv.URL, got) || got == "" {
		t.Errorf("server.address = %q, want the processor's host", got)
	}
}

// The trace context the processor receives must descend from the client
// span, so its server span nests under the attempt that made the call.
func TestTheProcessorReceivesTraceContextFromTheClientSpan(t *testing.T) {
	var received string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		received = r.Header.Get("traceparent")
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer srv.Close()

	provider, err := tracing.NewProvider(config.Tracing{SampleRatio: 1},
		tracing.Service{Name: "test"}, discardLogger())
	if err != nil {
		t.Fatalf("NewProvider: %v", err)
	}
	defer func() { _ = provider.Shutdown(context.Background()) }()

	// A parent span, as the gateway's request span would be.
	ctx, parent := provider.Tracer().Start(context.Background(), "GET /api/v1/processing/jobs")
	defer parent.End()

	c := tracedClient(t, srv.URL, provider.Tracer(), 1)
	_, _ = c.Process(ctx, request())

	if received == "" {
		t.Fatal("no trace context reached the processor")
	}
	parts := strings.Split(received, "-")
	if len(parts) != 4 || parts[1] != tracing.TraceID(ctx) {
		t.Errorf("traceparent %q does not carry the caller's trace %s", received, tracing.TraceID(ctx))
	}
}

// The span attributes never carry the address's path or the request body:
// only the host, the method and the status.
func TestTheClientSpanCarriesNoPathOrPayload(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer srv.Close()

	log := &spanLog{}
	c := tracedClient(t, srv.URL, log, 1)
	_, _ = c.Process(context.Background(), request())

	for key, value := range log.attrs {
		text, _ := value.(string)
		if strings.Contains(text, "/internal/") || strings.Contains(text, "11111111-1111-4111-8111-111111111111") {
			t.Errorf("attribute %s = %q carries a path or a request value", key, text)
		}
	}
}
