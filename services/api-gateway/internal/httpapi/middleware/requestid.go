package middleware

import (
	"net/http"

	"github.com/n0ah-n0wa/VitalMesh/services/api-gateway/internal/requestid"
)

// RequestID ensures every request has a request identifier and a
// correlation identifier (SPECIFICATIONS.md section 85). A well-formed
// client value is kept; anything else is replaced rather than rejected,
// because an unusable identifier must not turn valid work into a failure.
// A request that carries no correlation ID starts one, taking the request
// ID so that a single-hop call still correlates with itself. Both are
// stored in the request context, echoed in the response, and propagated to
// the processor by the outbound client.
func RequestID() Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			id := r.Header.Get(requestid.Header)
			if !requestid.Valid(id) {
				id = requestid.Generate()
			}
			correlation := r.Header.Get(requestid.CorrelationHeader)
			if !requestid.Valid(correlation) {
				correlation = id
			}
			w.Header().Set(requestid.Header, id)
			w.Header().Set(requestid.CorrelationHeader, correlation)
			ctx := requestid.NewContext(r.Context(), id)
			ctx = requestid.NewCorrelationContext(ctx, correlation)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}
