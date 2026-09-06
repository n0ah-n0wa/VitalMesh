package tracing

import (
	"context"
	"errors"
	"testing"
)

func TestNoopTracerAndTraceID(t *testing.T) {
	var tr Tracer = OrNoop(nil)
	ctx, span := tr.Start(context.Background(), "op")
	span.SetAttribute("k", "v")
	span.RecordError(errors.New("x"))
	span.End()
	if TraceID(ctx) != "" {
		t.Errorf("noop tracer set a trace id %q", TraceID(ctx))
	}
	if TraceID(WithTraceID(ctx, "abc")) != "abc" {
		t.Error("WithTraceID/TraceID round trip failed")
	}
}
