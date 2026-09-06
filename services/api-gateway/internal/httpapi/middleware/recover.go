package middleware

import (
	"errors"
	"log/slog"
	"net/http"
	"runtime/debug"

	"github.com/n0ah-n0wa/VitalMesh/services/api-gateway/internal/httpapi/respond"
)

// Recover turns a panic in a handler into a logged 500 with the generic error
// envelope, so that stack traces stay server-side. http.ErrAbortHandler is
// re-raised because net/http uses it to abort a response deliberately.
func Recover(logger *slog.Logger) Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			defer func() {
				p := recover()
				if p == nil {
					return
				}
				if err, ok := p.(error); ok && errors.Is(err, http.ErrAbortHandler) {
					panic(p)
				}
				logger.ErrorContext(r.Context(), "panic recovered",
					"panic", p,
					"stack", string(debug.Stack()),
				)
				respond.ErrorStatus(w, r, http.StatusInternalServerError, respond.InternalCode, respond.InternalMessage)
			}()
			next.ServeHTTP(w, r)
		})
	}
}
