package middleware

import (
	"net/http"

	"github.com/n0ah-n0wa/VitalMesh/services/api-gateway/internal/requestid"
)

// RequestID ensures every request has an identifier. A well-formed client
// value in the X-Request-ID header is kept; anything else is replaced by a
// generated one. The identifier is stored in the request context and echoed
// in the response header.
func RequestID() Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			id := r.Header.Get(requestid.Header)
			if !requestid.Valid(id) {
				id = requestid.Generate()
			}
			w.Header().Set(requestid.Header, id)
			next.ServeHTTP(w, r.WithContext(requestid.NewContext(r.Context(), id)))
		})
	}
}
