// Package app assembles the gateway from its parts and runs its lifecycle.
// It is the only package that knows every other package.
package app

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/n0ah-n0wa/VitalMesh/services/api-gateway/internal/config"
	"github.com/n0ah-n0wa/VitalMesh/services/api-gateway/internal/health"
	"github.com/n0ah-n0wa/VitalMesh/services/api-gateway/internal/httpapi"
	"github.com/n0ah-n0wa/VitalMesh/services/api-gateway/internal/httpapi/handler"
	"github.com/n0ah-n0wa/VitalMesh/services/api-gateway/internal/infra/postgres"
)

// ServiceName identifies the gateway in logs and health responses.
const ServiceName = "api-gateway"

// App is a fully wired gateway.
type App struct {
	cfg     config.Config
	logger  *slog.Logger
	pool    *pgxpool.Pool
	handler http.Handler
}

// New wires the application. The database pool connects lazily, so New
// succeeds even when PostgreSQL is down; readiness reports the outage.
func New(ctx context.Context, cfg config.Config, logger *slog.Logger, version string) (*App, error) {
	pool, err := postgres.Connect(ctx, cfg.Database)
	if err != nil {
		return nil, fmt.Errorf("database: %w", err)
	}

	readiness := health.NewReadiness(cfg.Readiness.Timeout, postgres.NewChecker(pool))
	healthHandler := handler.NewHealth(ServiceName, version, readiness, logger)

	return &App{
		cfg:     cfg,
		logger:  logger,
		pool:    pool,
		handler: httpapi.NewHandler(cfg.HTTP, logger, healthHandler),
	}, nil
}

// Handler returns the root HTTP handler, for in-process tests.
func (a *App) Handler() http.Handler { return a.handler }

// Run serves HTTP until ctx is cancelled and the server has shut down, then
// closes the database pool.
func (a *App) Run(ctx context.Context) error {
	defer a.pool.Close()

	a.logger.Info("starting", "addr", a.cfg.HTTP.Addr)
	if err := httpapi.Run(ctx, a.handler, a.cfg.HTTP); err != nil {
		return err
	}
	a.logger.Info("stopped")
	return nil
}
