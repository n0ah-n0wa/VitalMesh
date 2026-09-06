// Package app assembles the gateway from its parts and runs its lifecycle.
// It is the only package that knows every other package.
package app

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/n0ah-n0wa/VitalMesh/services/api-gateway/internal/auth"
	"github.com/n0ah-n0wa/VitalMesh/services/api-gateway/internal/authz"
	"github.com/n0ah-n0wa/VitalMesh/services/api-gateway/internal/config"
	"github.com/n0ah-n0wa/VitalMesh/services/api-gateway/internal/domain"
	"github.com/n0ah-n0wa/VitalMesh/services/api-gateway/internal/health"
	"github.com/n0ah-n0wa/VitalMesh/services/api-gateway/internal/httpapi"
	"github.com/n0ah-n0wa/VitalMesh/services/api-gateway/internal/httpapi/handler"
	"github.com/n0ah-n0wa/VitalMesh/services/api-gateway/internal/httpapi/middleware"
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
	tokens := auth.NewTokens(cfg.Auth.JWT, nil)
	authService, err := auth.NewService(
		postgres.NewUsers(pool),
		auditor{repo: postgres.NewAudit(pool)},
		auth.NewHasher(cfg.Auth.Password),
		tokens,
		logger,
	)
	if err != nil {
		pool.Close()
		return nil, fmt.Errorf("auth: %w", err)
	}

	handlers := httpapi.Handlers{
		Health:       handler.NewHealth(ServiceName, version, readiness, logger),
		Auth:         handler.NewAuth(authService, logger),
		Authenticate: middleware.Authenticate(tokens, logger),
		Policy:       authz.Default(),
	}
	root, err := httpapi.NewHandler(cfg.HTTP, logger, handlers)
	if err != nil {
		pool.Close()
		return nil, fmt.Errorf("routes: %w", err)
	}
	return &App{
		cfg:     cfg,
		logger:  logger,
		pool:    pool,
		handler: root,
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

// auditor adapts the audit repository to the events the auth service
// records, keeping auth free of persistence types.
type auditor struct {
	repo *postgres.Audit
}

func (a auditor) RecordLogin(ctx context.Context, userID uuid.UUID, requestID string) error {
	_, err := a.repo.Append(ctx, postgres.NewAuditEntry{
		ActorID:      &userID,
		ActorType:    domain.ActorUser,
		Action:       domain.AuditLogin,
		ResourceType: "user",
		ResourceID:   &userID,
		RequestID:    requestID,
	})
	return err
}
