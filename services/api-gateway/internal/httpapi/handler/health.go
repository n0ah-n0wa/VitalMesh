// Package handler contains the HTTP handlers. Each one translates a request
// into application-layer calls and the result into a wire model; no business
// rules live here.
package handler

import (
	"log/slog"
	"net/http"

	"github.com/n0ah-n0wa/VitalMesh/services/api-gateway/internal/health"
	"github.com/n0ah-n0wa/VitalMesh/services/api-gateway/internal/httpapi/model"
	"github.com/n0ah-n0wa/VitalMesh/services/api-gateway/internal/httpapi/respond"
)

// Health serves the liveness and readiness endpoints.
type Health struct {
	service   string
	version   string
	readiness *health.Readiness
	logger    *slog.Logger
}

// NewHealth returns a Health handler reporting the given service identity.
func NewHealth(service, version string, readiness *health.Readiness, logger *slog.Logger) *Health {
	return &Health{service: service, version: version, readiness: readiness, logger: logger}
}

// Live reports process liveness. It never consults dependencies, so a
// dependency outage cannot trigger a restart.
func (h *Health) Live(w http.ResponseWriter, r *http.Request) {
	body := model.Health{
		Status:  model.StatusOK,
		Service: h.service,
		Version: h.version,
	}
	if err := respond.JSON(w, http.StatusOK, body); err != nil {
		respond.Error(w, r, h.logger, err)
	}
}

// Ready reports whether every required dependency is usable. Failure causes
// are logged, not returned, so the unauthenticated endpoint reveals nothing
// about the infrastructure.
func (h *Health) Ready(w http.ResponseWriter, r *http.Request) {
	report := h.readiness.Check(r.Context())

	body := model.Readiness{
		Status:  model.StatusReady,
		Service: h.service,
		Version: h.version,
		Checks:  make([]model.Check, 0, len(report.Checks)),
	}
	status := http.StatusOK
	if !report.Ready {
		body.Status = model.StatusNotReady
		status = http.StatusServiceUnavailable
	}

	for _, c := range report.Checks {
		check := model.Check{Name: c.Name, Status: model.StatusOK}
		if c.Err != nil {
			check.Status = model.StatusFail
			h.logger.WarnContext(r.Context(), "readiness check failed", "check", c.Name, "error", c.Err)
		}
		body.Checks = append(body.Checks, check)
	}

	if err := respond.JSON(w, status, body); err != nil {
		respond.Error(w, r, h.logger, err)
	}
}
