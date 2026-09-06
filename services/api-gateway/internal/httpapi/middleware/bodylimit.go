package middleware

import (
	"net/http"

	"github.com/n0ah-n0wa/VitalMesh/services/api-gateway/internal/httpapi/request"
	"github.com/n0ah-n0wa/VitalMesh/services/api-gateway/internal/httpapi/respond"
)

// BodyLimit rejects request bodies larger than maxBytes. A declared
// Content-Length above the limit is rejected before any byte is read;
// otherwise the body is capped so that reads past the limit fail and are
// reported by request.DecodeJSON.
func BodyLimit(maxBytes int64) Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.ContentLength > maxBytes {
				e := request.ErrBodyTooLarge(maxBytes)
				respond.ErrorStatus(w, r, http.StatusRequestEntityTooLarge, e.Code, e.Message)
				return
			}
			if r.Body != nil {
				r.Body = http.MaxBytesReader(w, r.Body, maxBytes)
			}
			next.ServeHTTP(w, r)
		})
	}
}
