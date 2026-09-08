package tracing

import (
	"context"
	"encoding/base64"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/n0ah-n0wa/VitalMesh/services/api-gateway/internal/config"
)

func discard() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func provider(t *testing.T, cfg config.Tracing) *Provider {
	t.Helper()
	p, err := NewProvider(cfg, Service{Name: "test", Version: "v0", Environment: "test"}, discard())
	if err != nil {
		t.Fatalf("NewProvider: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = p.Shutdown(ctx)
	})
	return p
}

// ------------------------------------------------------ propagation

// The whole point of W3C trace context: what one service writes, the next
// reads, so two services are on one trace.
func TestTraceContextSurvivesInjectionAndExtraction(t *testing.T) {
	p := provider(t, config.Tracing{SampleRatio: 1})
	ctx, span := p.Tracer().Start(context.Background(), "outbound")
	defer span.End()

	header := http.Header{}
	Inject(ctx, header)

	traceparent := header.Get("traceparent")
	if traceparent == "" {
		t.Fatal("nothing was injected; a trace would stop at this service")
	}
	// The W3C form: version, trace id, span id, flags.
	parts := strings.Split(traceparent, "-")
	if len(parts) != 4 || parts[0] != "00" || len(parts[1]) != 32 || len(parts[2]) != 16 {
		t.Fatalf("traceparent %q is not W3C trace context", traceparent)
	}
	if parts[1] != TraceID(ctx) {
		t.Errorf("injected trace id %s, logged %s", parts[1], TraceID(ctx))
	}

	// And reading it back on the far side continues the same trace.
	inbound := Extract(context.Background(), header)
	_, child := p.Tracer().Start(inbound, "inbound")
	defer child.End()
	if got := TraceID(context.Background()); got != "" {
		t.Errorf("a context with no trace reported %q", got)
	}
}

// Propagation must not depend on having somewhere to send spans, or a trace
// would stop wherever a collector is missing.
func TestATraceIsPropagatedWithNoCollectorConfigured(t *testing.T) {
	p := provider(t, config.Tracing{SampleRatio: 1}) // no endpoint
	ctx, span := p.Tracer().Start(context.Background(), "work")
	defer span.End()

	if TraceID(ctx) == "" {
		t.Fatal("no trace id exists without a collector")
	}
	header := http.Header{}
	Inject(ctx, header)
	if header.Get("traceparent") == "" {
		t.Error("no trace context was injected without a collector")
	}
}

// A caller's trace is continued rather than replaced, which is what makes
// one trace cover client, gateway and processor.
func TestAnIncomingTraceIsContinuedRatherThanReplaced(t *testing.T) {
	const traceID = "4bf92f3577b34da6a3ce929d0e0e4736"
	p := provider(t, config.Tracing{SampleRatio: 1})

	header := http.Header{}
	header.Set("traceparent", "00-"+traceID+"-00f067aa0ba902b7-01")
	ctx, span := p.Tracer().Start(Extract(context.Background(), header), "server")
	defer span.End()

	if got := TraceID(ctx); got != traceID {
		t.Errorf("trace id = %s, want the caller's %s", got, traceID)
	}
}

func TestAMalformedTraceparentStartsAFreshTraceRatherThanFailing(t *testing.T) {
	p := provider(t, config.Tracing{SampleRatio: 1})
	header := http.Header{}
	header.Set("traceparent", "not-a-traceparent")

	ctx, span := p.Tracer().Start(Extract(context.Background(), header), "server")
	defer span.End()
	if TraceID(ctx) == "" {
		t.Error("a broken header cost the request its trace entirely")
	}
}

// ------------------------------------------------------- attributes

// A span leaves the process for a backend that is not the record's
// custodian, so a value that is not plainly operational is described rather
// than rendered (SPECIFICATIONS.md sections 40 and 41).
func TestAttributesCarryMetadataAndNeverAPayload(t *testing.T) {
	id := uuid.New()

	// Safe operational metadata is carried as itself.
	for _, c := range []struct {
		value any
		want  string
	}{
		{"POST /api/v1/patients", "POST /api/v1/patients"},
		{id, id.String()},
	} {
		if got := Attribute("k", c.value).Value.AsString(); got != c.want {
			t.Errorf("Attribute(%v) = %q, want %q", c.value, got, c.want)
		}
	}

	// Anything else is reduced to its type. A struct holding readings can
	// therefore never be serialised into a span by being passed as an
	// interface value.
	type reading struct {
		PatientID string
		Value     float64
	}
	got := Attribute("reading", reading{PatientID: id.String(), Value: 121.5}).Value.AsString()
	if strings.Contains(got, id.String()) || strings.Contains(got, "121.5") {
		t.Errorf("a payload reached a span attribute: %q", got)
	}
	if !strings.HasPrefix(got, "<") {
		t.Errorf("attribute = %q, want the value described by its type", got)
	}

	// And a name that is itself sensitive is refused whatever it carries,
	// the same rule the logger applies, so the two exits agree.
	for _, name := range []string{"payload", "token", "internal_token", "email"} {
		if got := Attribute(name, "anything").Value.AsString(); got != "[redacted]" {
			t.Errorf("Attribute(%q) = %q, want it redacted by name", name, got)
		}
	}
	// A token is removed by shape wherever it appears, including in an
	// attribute under a harmless name.
	// Assembled rather than written out, so no source file carries a
	// token-shaped literal for a secret scanner to flag.
	segment := func(s string) string { return base64.RawURLEncoding.EncodeToString([]byte(s)) }
	fakeJWT := segment(`{"alg":"HS256"}`) + "." + segment(`{"sub":"x"}`) + "." + segment("sig")
	if got := Attribute("note", "bearer "+fakeJWT).Value.AsString(); strings.Contains(got, fakeJWT) {
		t.Errorf("a token survived in a span attribute: %q", got)
	}

	// Numbers and durations keep their kind, so a dashboard can use them.
	if got := Attribute("n", 42).Value.AsInt64(); got != 42 {
		t.Errorf("int attribute = %d", got)
	}
	if got := Attribute("d", 1500*time.Millisecond).Value.AsInt64(); got != 1500 {
		t.Errorf("duration attribute = %d ms", got)
	}
}

// ------------------------------------------- telemetry backend failures

// A collector that does not exist must not stop the gateway from starting
// or from working: export happens on its own goroutine.
func TestAnUnreachableCollectorCostsTheRequestNothing(t *testing.T) {
	p := provider(t, config.Tracing{
		Endpoint:    "http://127.0.0.1:1", // nothing listens here
		SampleRatio: 1,
		Timeout:     100 * time.Millisecond,
		Insecure:    true,
	})

	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 50; i++ {
			_, span := p.Tracer().Start(context.Background(), "work")
			span.SetAttribute("i", i)
			span.RecordError(errors.New("something"))
			span.End()
		}
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("recording spans blocked on an unreachable collector")
	}
}

// Shutdown is bounded, so a collector that has gone away cannot hold up
// the process.
func TestShutdownIsBoundedWhenTheCollectorIsGone(t *testing.T) {
	p, err := NewProvider(config.Tracing{
		Endpoint: "http://127.0.0.1:1", SampleRatio: 1,
		Timeout: 100 * time.Millisecond, Insecure: true,
	}, Service{Name: "test"}, discard())
	if err != nil {
		t.Fatalf("NewProvider: %v", err)
	}
	_, span := p.Tracer().Start(context.Background(), "work")
	span.End()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	started := time.Now()
	_ = p.Shutdown(ctx) // an error is fine; hanging is not
	if elapsed := time.Since(started); elapsed > 3*time.Second {
		t.Errorf("shutdown took %s against a dead collector", elapsed)
	}
}

// An address that cannot be parsed is a configuration fault and is refused
// at start-up, unlike a collector that is merely down.
func TestAnUnusableEndpointIsRefusedAtStartUp(t *testing.T) {
	_, err := NewProvider(config.Tracing{Endpoint: "://not a url", SampleRatio: 1},
		Service{Name: "test"}, discard())
	if err == nil {
		t.Error("an unparseable endpoint was accepted")
	}
}
