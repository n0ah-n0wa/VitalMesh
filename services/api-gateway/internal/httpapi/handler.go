package httpapi

import (
	"log/slog"
	"net/http"

	"github.com/n0ah-n0wa/VitalMesh/services/api-gateway/internal/config"
	"github.com/n0ah-n0wa/VitalMesh/services/api-gateway/internal/httpapi/handler"
	"github.com/n0ah-n0wa/VitalMesh/services/api-gateway/internal/httpapi/middleware"
)

// APIv1 is the prefix of the current public API version. Breaking changes
// require a new prefix.
const APIv1 = "/api/v1"

// NewHandler assembles the route table and wraps it in the middleware chain.
// It is the single place where routes are declared. Feature handlers register
// on rt.Group(APIv1) as they are implemented.
func NewHandler(cfg config.HTTP, logger *slog.Logger, healthHandler *handler.Health) http.Handler {
	rt := NewRouter(logger)
	rt.HandleFunc(http.MethodGet, "/health", healthHandler.Live)
	rt.HandleFunc(http.MethodGet, "/ready", healthHandler.Ready)

	return Wrap(cfg, logger, rt)
}

// Wrap applies the standard middleware chain to h, outermost first: request
// identification, security headers, request logging, the request timeout,
// panic recovery and the body size limit.
func Wrap(cfg config.HTTP, logger *slog.Logger, h http.Handler) http.Handler {
	return middleware.Chain(h,
		middleware.RequestID(),
		middleware.SecureHeaders(),
		middleware.Logging(logger),
		middleware.Timeout(cfg.RequestTimeout),
		middleware.Recover(logger),
		middleware.BodyLimit(cfg.MaxBodyBytes),
	)
}
