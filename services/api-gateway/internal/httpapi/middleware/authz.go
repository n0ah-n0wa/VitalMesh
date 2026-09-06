package middleware

import (
	"log/slog"
	"net/http"

	"github.com/n0ah-n0wa/VitalMesh/services/api-gateway/internal/authz"
	"github.com/n0ah-n0wa/VitalMesh/services/api-gateway/internal/httpapi/respond"
)

// Require lets a request through only when the principal established by
// Authenticate holds permission under policy. Without a principal it
// answers 401, as Authenticate would, so a route that is accidentally
// registered without Authenticate still fails closed. Denials answer 403
// PERMISSION_DENIED.
func Require(policy *authz.Policy, permission authz.Permission, logger *slog.Logger) Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if err := policy.Authorize(r.Context(), logger, permission); err != nil {
				respond.Error(w, r, logger, err)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}
