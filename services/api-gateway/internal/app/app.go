// Package app assembles the gateway from its parts and runs its lifecycle.
// It is the only package that knows every other package.
package app

import (
	"context"
	"log/slog"
	"net/http"

	"github.com/n0ah-n0wa/VitalMesh/services/api-gateway/internal/config"
	"github.com/n0ah-n0wa/VitalMesh/services/api-gateway/internal/health"
	"github.com/n0ah-n0wa/VitalMesh/services/api-gateway/internal/httpapi"
	"github.com/n0ah-n0wa/VitalMesh/services/api-gateway/internal/httpapi/handler"
)

// ServiceName identifies the gateway in logs and health responses.
const ServiceName = "api-gateway"

// App is a fully wired gateway.
type App struct {
	cfg     config.Config
	logger  *slog.Logger
	handler http.Handler
}

// New wires the application. Readiness has no checkers yet; each
// infrastructure dependency registers one when it is introduced.
func New(cfg config.Config, logger *slog.Logger, version string) *App {
	readiness := health.NewReadiness(cfg.Readiness.Timeout)
	healthHandler := handler.NewHealth(ServiceName, version, readiness, logger)

	return &App{
		cfg:     cfg,
		logger:  logger,
		handler: httpapi.NewHandler(cfg.HTTP, logger, healthHandler),
	}
}

// Handler returns the root HTTP handler, for in-process tests.
func (a *App) Handler() http.Handler { return a.handler }

// Run serves HTTP until ctx is cancelled and the server has shut down.
func (a *App) Run(ctx context.Context) error {
	a.logger.Info("starting", "addr", a.cfg.HTTP.Addr)
	if err := httpapi.Run(ctx, a.handler, a.cfg.HTTP); err != nil {
		return err
	}
	a.logger.Info("stopped")
	return nil
}
