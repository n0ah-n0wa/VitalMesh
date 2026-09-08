package tracing

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/propagation"
	sdkresource "go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.26.0"
	oteltrace "go.opentelemetry.io/otel/trace"

	"github.com/n0ah-n0wa/VitalMesh/services/api-gateway/internal/config"
	"github.com/n0ah-n0wa/VitalMesh/services/api-gateway/internal/observability/redact"
)

// Provider is the OpenTelemetry binding of this package's port
// (SPECIFICATIONS.md section 40).
//
// # Failing gracefully
//
// A telemetry backend is never on the critical path of a request. Spans are
// handed to a batch processor and exported on its own goroutine, so an
// export that is slow, refused or impossible costs the request nothing.
// A collector that cannot be reached produces one log line rather than a
// failed request, and the spans it could not take are dropped: telemetry
// that degrades a service is worse than no telemetry.
//
// # Propagation without an exporter
//
// Propagation and export are separate concerns. With no endpoint
// configured, [NewProvider] still installs the W3C propagator and still
// builds a real tracer, so trace ids exist, reach the logs, and are
// injected into the call to the processor. A trace therefore spans both
// services whether or not anyone is collecting it. What is not done is
// exporting the spans.
type Provider struct {
	tracer   oteltrace.Tracer
	shutdown func(context.Context) error
}

// Service identifies this process in every span it produces.
type Service struct {
	Name        string
	Version     string
	Environment string
}

// NewProvider builds the tracer. It never fails because a collector is
// unreachable: the exporter connects lazily, so a collector that is down at
// start-up is an outage rather than a configuration error. Only an address
// that cannot be parsed is refused, and configuration has already checked
// that.
func NewProvider(cfg config.Tracing, svc Service, logger *slog.Logger) (*Provider, error) {
	// The propagator is installed whether or not spans are exported, so
	// that trace context crosses this service either way.
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{},
		propagation.Baggage{},
	))
	// An exporter that cannot ship its spans must not shout on every
	// attempt; the handler reports the first failure of an outage.
	otel.SetErrorHandler(newErrorHandler(logger))

	options := []sdktrace.TracerProviderOption{
		sdktrace.WithResource(resourceFor(svc)),
		// ParentBased honours a decision made upstream, so a trace a client
		// chose to sample is not truncated here, and only a trace that
		// starts with this service is sampled by ratio.
		sdktrace.WithSampler(sdktrace.ParentBased(sdktrace.TraceIDRatioBased(cfg.SampleRatio))),
	}

	// A real provider is built whether or not there is anywhere to send
	// spans. Without one, no trace id would exist, nothing would be
	// injected into outgoing requests, and a trace would stop at this
	// service; propagation would then depend on having a backend, which is
	// exactly backwards. With no endpoint the spans are simply recorded and
	// dropped.
	if cfg.Enabled() {
		exporter, err := newExporter(cfg)
		if err != nil {
			return nil, err
		}
		// Batching is what keeps export off the request path.
		options = append(options,
			sdktrace.WithBatcher(exporter, sdktrace.WithExportTimeout(cfg.Timeout)))
	}

	provider := sdktrace.NewTracerProvider(options...)
	otel.SetTracerProvider(provider)

	return &Provider{
		tracer:   provider.Tracer(svc.Name),
		shutdown: provider.Shutdown,
	}, nil
}

// newExporter builds the OTLP client. A collector that is down does not
// fail here: the client connects lazily, so an unreachable collector is an
// outage rather than a configuration error.
func newExporter(cfg config.Tracing) (*otlptrace.Exporter, error) {
	endpoint, err := url.Parse(cfg.Endpoint)
	if err != nil {
		return nil, errors.New("tracing endpoint: must be a valid URL")
	}
	options := []otlptracehttp.Option{
		otlptracehttp.WithEndpoint(endpoint.Host),
		otlptracehttp.WithTimeout(cfg.Timeout),
	}
	if endpoint.Path != "" && endpoint.Path != "/" {
		options = append(options, otlptracehttp.WithURLPath(endpoint.Path))
	}
	if cfg.Insecure || endpoint.Scheme == "http" {
		options = append(options, otlptracehttp.WithInsecure())
	}
	exporter := otlptracehttp.NewUnstarted(options...)
	if err := exporter.Start(context.Background()); err != nil {
		return nil, fmt.Errorf("tracing exporter: %w", err)
	}
	return exporter, nil
}

func resourceFor(svc Service) *sdkresource.Resource {
	return sdkresource.NewSchemaless(
		semconv.ServiceName(svc.Name),
		semconv.ServiceVersion(svc.Version),
		semconv.DeploymentEnvironment(svc.Environment),
	)
}

// Tracer returns the port implementation the application uses.
func (p *Provider) Tracer() Tracer { return otelTracer{tracer: p.tracer} }

// Shutdown flushes what has been recorded and stops the exporter. A
// deadline in ctx bounds it, because shutdown must not hang on a collector
// that has gone away.
func (p *Provider) Shutdown(ctx context.Context) error { return p.shutdown(ctx) }

// otelTracer adapts the OpenTelemetry tracer to this package's port.
type otelTracer struct{ tracer oteltrace.Tracer }

func (t otelTracer) Start(ctx context.Context, name string) (context.Context, Span) {
	ctx, span := t.tracer.Start(ctx, name)
	// The trace id is put where the logger can find it, so a log line and a
	// span can be joined without the logger knowing about OpenTelemetry.
	if id := span.SpanContext().TraceID(); id.IsValid() {
		ctx = WithTraceID(ctx, id.String())
	}
	return ctx, otelSpan{span: span}
}

type otelSpan struct{ span oteltrace.Span }

// SetAttribute records safe operational metadata. Values are constrained to
// the kinds an attribute can hold; anything else is recorded as its type
// rather than its content, so a payload cannot reach a span by being passed
// as an interface (SPECIFICATIONS.md section 40). Strings pass through the
// same redaction the logger applies, so the two exits agree.
func (s otelSpan) SetAttribute(key string, value any) {
	s.span.SetAttributes(Attribute(key, value))
}

func (s otelSpan) SetName(name string) { s.span.SetName(name) }

// RecordError marks the span failed. The message is scrubbed first: an
// error is the likeliest place for a token or a payload to be formatted
// into text, and a span leaves the process for a backend that is not the
// record's custodian.
func (s otelSpan) RecordError(err error) {
	if err == nil {
		return
	}
	message := redact.Scrub(err.Error())
	s.span.RecordError(errors.New(message))
	s.span.SetStatus(codes.Error, message)
}

func (s otelSpan) End() { s.span.End() }

// Attribute converts a value to a span attribute. It is exported so that
// the same rule applies wherever attributes are made.
func Attribute(key string, value any) attribute.KeyValue {
	// The same name rule as the logger: a value under a name that is a
	// credential or a personal fact is never recorded, whatever it is.
	if redact.Sensitive(key) {
		return attribute.String(key, redact.Redacted)
	}
	switch v := value.(type) {
	case string:
		return attribute.String(key, redact.Scrub(v))
	case bool:
		return attribute.Bool(key, v)
	case int:
		return attribute.Int(key, v)
	case int64:
		return attribute.Int64(key, v)
	case float64:
		return attribute.Float64(key, v)
	case time.Duration:
		return attribute.Int64(key, v.Milliseconds())
	case fmt.Stringer:
		// Identifiers reach spans this way: a UUID is safe operational
		// metadata, and a span is not a metric label, so cardinality is not
		// a concern here.
		return attribute.String(key, redact.Scrub(v.String()))
	default:
		// Anything else is described rather than rendered, so a struct
		// holding a payload cannot be serialised into a span.
		return attribute.String(key, fmt.Sprintf("<%T>", value))
	}
}

// Inject writes the current trace context into outgoing request headers as
// W3C traceparent, which is what makes one trace span two services.
func Inject(ctx context.Context, header http.Header) {
	otel.GetTextMapPropagator().Inject(ctx, propagation.HeaderCarrier(header))
}

// Extract reads W3C trace context from incoming request headers, so a span
// started afterwards continues the caller's trace instead of beginning a
// new one.
func Extract(ctx context.Context, header http.Header) context.Context {
	return otel.GetTextMapPropagator().Extract(ctx, propagation.HeaderCarrier(header))
}

// errorHandler reports exporter failures once per outage rather than once
// per attempt, so a collector that is down costs one log line.
type errorHandler struct {
	logger *slog.Logger
	failed *bool
}

func newErrorHandler(logger *slog.Logger) otel.ErrorHandler {
	failed := false
	return errorHandler{logger: logger, failed: &failed}
}

func (h errorHandler) Handle(err error) {
	if err == nil || *h.failed {
		return
	}
	*h.failed = true
	h.logger.Warn("trace export failed; spans are being dropped and requests are unaffected", "error", err)
}
