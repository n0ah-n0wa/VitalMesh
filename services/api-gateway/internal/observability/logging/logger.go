// Package logging configures structured logging. Every record carries the
// service identity; records logged with a context that holds a request ID
// or a trace ID carry them too.
package logging

import (
	"context"
	"io"
	"log/slog"
	"slices"

	"github.com/n0ah-n0wa/VitalMesh/services/api-gateway/internal/config"
	"github.com/n0ah-n0wa/VitalMesh/services/api-gateway/internal/observability/tracing"
	"github.com/n0ah-n0wa/VitalMesh/services/api-gateway/internal/requestid"
)

// Service identifies the running process in every record.
type Service struct {
	Name        string
	Version     string
	Environment string
}

// New returns a logger writing to w. Time and message attributes are named
// "timestamp" and "message" to match the repository's log schema.
func New(w io.Writer, cfg config.Log, svc Service) *slog.Logger {
	opts := &slog.HandlerOptions{Level: cfg.Level, ReplaceAttr: renameStandardAttrs}

	var h slog.Handler
	if cfg.Format == config.LogFormatText {
		h = slog.NewTextHandler(w, opts)
	} else {
		h = slog.NewJSONHandler(w, opts)
	}

	h = h.WithAttrs([]slog.Attr{
		slog.String("service", svc.Name),
		slog.String("version", svc.Version),
		slog.String("environment", svc.Environment),
	})
	return slog.New(contextHandler{root: h, derived: h})
}

func renameStandardAttrs(groups []string, a slog.Attr) slog.Attr {
	if len(groups) > 0 {
		return a
	}
	switch a.Key {
	case slog.TimeKey:
		a.Key = "timestamp"
	case slog.MessageKey:
		a.Key = "message"
	}
	return a
}

// contextHandler adds request-scoped attributes taken from the context as
// top-level fields, even when the logger has been derived with WithGroup.
// It keeps the root handler and replays derivations on top of the
// context attributes, because attributes added to a record after WithGroup
// would otherwise be nested inside that group.
type contextHandler struct {
	root    slog.Handler
	derived slog.Handler
	ops     []func(slog.Handler) slog.Handler
}

func (h contextHandler) Enabled(ctx context.Context, level slog.Level) bool {
	return h.derived.Enabled(ctx, level)
}

func (h contextHandler) Handle(ctx context.Context, r slog.Record) error {
	var attrs []slog.Attr
	if id := requestid.FromContext(ctx); id != "" {
		attrs = append(attrs, slog.String("request_id", id))
	}
	if id := tracing.TraceID(ctx); id != "" {
		attrs = append(attrs, slog.String("trace_id", id))
	}
	if len(attrs) == 0 {
		return h.derived.Handle(ctx, r)
	}
	target := h.root.WithAttrs(attrs)
	for _, op := range h.ops {
		target = op(target)
	}
	return target.Handle(ctx, r)
}

func (h contextHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return h.derive(h.derived.WithAttrs(attrs), func(x slog.Handler) slog.Handler {
		return x.WithAttrs(attrs)
	})
}

func (h contextHandler) WithGroup(name string) slog.Handler {
	return h.derive(h.derived.WithGroup(name), func(x slog.Handler) slog.Handler {
		return x.WithGroup(name)
	})
}

func (h contextHandler) derive(derived slog.Handler, op func(slog.Handler) slog.Handler) contextHandler {
	return contextHandler{
		root:    h.root,
		derived: derived,
		ops:     append(slices.Clip(h.ops), op),
	}
}
