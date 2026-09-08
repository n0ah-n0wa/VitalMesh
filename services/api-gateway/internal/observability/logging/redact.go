package logging

import (
	"log/slog"

	"github.com/n0ah-n0wa/VitalMesh/services/api-gateway/internal/observability/redact"
)

// The rules themselves live in [redact], shared with tracing, so that logs
// and spans cannot drift apart in what they refuse to carry. This file is
// only the slog binding: every attribute passes through it on its way to
// the handler, so a credential logged by mistake is dropped by the logger
// rather than written to disk and shipped to a log store.

// sanitizeAttr applies the shared rules to one attribute. It runs at every
// depth, including attributes a logger was derived with, because a
// credential is no less a credential for being inside a group.
func sanitizeAttr(a slog.Attr) slog.Attr {
	if redact.Sensitive(a.Key) {
		return slog.String(a.Key, redact.Redacted)
	}
	// Resolve first: a value that redacts itself, such as config.Secret,
	// has already replaced its contents by this point and the result is
	// scrubbed like any other string.
	value := a.Value.Resolve()
	switch value.Kind() {
	case slog.KindString:
		a.Value = slog.StringValue(redact.Scrub(value.String()))
	case slog.KindAny:
		if err, ok := value.Any().(error); ok && err != nil {
			// An error reaches the output as its message, so scrub that
			// message rather than the value it was built from.
			a.Value = slog.StringValue(redact.Scrub(err.Error()))
			return a
		}
		a.Value = value
	default:
		a.Value = value
	}
	return a
}
