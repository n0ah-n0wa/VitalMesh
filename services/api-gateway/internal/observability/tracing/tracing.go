// Package tracing is the distributed tracing port of the application. Code
// opens spans through a Tracer; the observability phase binds OpenTelemetry
// to it and propagates W3C trace context. Until then Noop keeps every call
// free, and the trace ID carried by a context (when a real tracer set one)
// is available to the logger.
//
// Attributes must be safe operational metadata: identifiers are fine,
// payload contents are not (SPECIFICATIONS.md section 40).
package tracing

import "context"

// Tracer starts spans.
type Tracer interface {
	// Start opens a span named name as a child of any span in ctx and
	// returns the context carrying it. The caller must End the span.
	Start(ctx context.Context, name string) (context.Context, Span)
}

// Span is one unit of traced work.
type Span interface {
	// SetAttribute records a key/value pair on the span.
	SetAttribute(key string, value any)
	// RecordError marks the span as failed with err. A nil err is ignored.
	RecordError(err error)
	End()
}

// Noop traces nothing.
type Noop struct{}

func (Noop) Start(ctx context.Context, _ string) (context.Context, Span) { return ctx, noopSpan{} }

type noopSpan struct{}

func (noopSpan) SetAttribute(string, any) {}
func (noopSpan) RecordError(error)        {}
func (noopSpan) End()                     {}

// OrNoop returns t, or Noop when t is nil, so callers never check.
func OrNoop(t Tracer) Tracer {
	if t == nil {
		return Noop{}
	}
	return t
}

type traceIDKey struct{}

// WithTraceID returns a copy of ctx carrying id. Tracer implementations
// call it from Start so that logs can be correlated with traces.
func WithTraceID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, traceIDKey{}, id)
}

// TraceID returns the trace ID carried by ctx, or "" when there is none.
func TraceID(ctx context.Context) string {
	id, _ := ctx.Value(traceIDKey{}).(string)
	return id
}
