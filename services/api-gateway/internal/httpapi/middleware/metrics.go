package middleware

import (
	"net/http"
	"time"

	"github.com/n0ah-n0wa/VitalMesh/services/api-gateway/internal/observability/metrics"
)

// Metrics records every request's method, matched route, status and
// duration through rec (SPECIFICATIONS.md section 41: request count,
// latency and errors follow from these). The route is the registered
// pattern, so the label set stays bounded.
func Metrics(rec metrics.Recorder) Middleware {
	rec = metrics.OrNoop(rec)
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()
			r, holder := ensureRouteHolder(r)
			sw := &statusWriter{ResponseWriter: w, status: http.StatusOK}

			next.ServeHTTP(sw, r)

			rec.HTTPRequest(r.Method, holder.route(), sw.status, time.Since(start))
		})
	}
}
