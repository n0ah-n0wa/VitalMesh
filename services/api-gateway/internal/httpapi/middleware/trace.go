package middleware

import (
	"net/http"

	"github.com/n0ah-n0wa/VitalMesh/services/api-gateway/internal/observability/metrics"
	"github.com/n0ah-n0wa/VitalMesh/services/api-gateway/internal/observability/tracing"
	"github.com/n0ah-n0wa/VitalMesh/services/api-gateway/internal/requestid"
)

// Trace continues the caller's trace, or begins one, and opens a span for
// the request (SPECIFICATIONS.md section 40).
//
// W3C trace context arrives in the `traceparent` header. Reading it before
// the span starts is what joins this service to a trace that began in the
// client, so a single trace covers client, gateway, processor and the
// dependencies each of them touches. A request without the header starts a
// new trace here.
//
// # Attributes
//
// Only safe operational metadata: the method, the matched route pattern,
// the status, and the request and correlation identifiers that already tie
// the logs together. Never a path, a query string, a header or a body. A
// path carries resource identifiers and a body carries patient data, and a
// span is shipped to a telemetry backend that is not the record's custodian
// (section 41).
func Trace(tracer tracing.Tracer) Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if isProbe(r.URL.Path) {
				// A liveness probe every few seconds per replica is not an
				// important request; traced, it would outnumber real traffic
				// in any trace store and bury the traces that matter. Probes
				// are visible in metrics and logs, which is enough.
				next.ServeHTTP(w, r)
				return
			}
			ctx := tracing.Extract(r.Context(), r.Header)
			r, holder := ensureRouteHolder(r.WithContext(ctx))

			ctx, span := tracer.Start(r.Context(), "http.request")
			defer span.End()

			sw := &statusWriter{ResponseWriter: w, status: http.StatusOK}
			next.ServeHTTP(sw, r.WithContext(ctx))

			// The route is only known once the router has matched, so the
			// name and the attributes are set after the handler rather than
			// before. Naming a span by its route is what keeps a trace
			// readable; naming it by its path would make every request its
			// own operation and leak identifiers into the name.
			// The method is the one value here a client controls, so it is
			// bounded the same way the metrics bound it.
			method := metrics.Method(r.Method)
			span.SetName(method + " " + holder.route())
			span.SetAttribute("http.request.method", method)
			span.SetAttribute("http.route", holder.route())
			span.SetAttribute("http.response.status_code", sw.status)
			if id := requestid.FromContext(ctx); id != "" {
				span.SetAttribute("vitalmesh.request_id", id)
			}
			if id := requestid.CorrelationFromContext(ctx); id != "" {
				span.SetAttribute("vitalmesh.correlation_id", id)
			}
		})
	}
}

// isProbe names the endpoints the platform polls rather than a client
// calls. They are unversioned and unauthenticated, and they are the only
// requests deliberately left out of tracing.
func isProbe(path string) bool {
	switch path {
	case "/health", "/ready", "/metrics":
		return true
	}
	return false
}
