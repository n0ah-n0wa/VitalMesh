// Package health implements the liveness and readiness endpoints.
package health

import (
	"encoding/json"
	"net/http"
)

// Response is the JSON body returned by the health endpoints.
type Response struct {
	Status  string `json:"status"`
	Service string `json:"service"`
	Version string `json:"version"`
}

// Handler serves liveness and readiness checks for one service.
type Handler struct {
	service string
	version string
}

// NewHandler returns a Handler reporting the given service name and version.
func NewHandler(service, version string) *Handler {
	return &Handler{service: service, version: version}
}

// Live reports process liveness. It must never depend on external systems.
func (h *Handler) Live(w http.ResponseWriter, _ *http.Request) {
	h.write(w)
}

// Ready reports readiness to serve traffic. No external dependencies are wired
// yet, so the service is ready as soon as it accepts connections. Dependency
// checks are added together with the PostgreSQL and Redis integrations.
func (h *Handler) Ready(w http.ResponseWriter, _ *http.Request) {
	h.write(w)
}

func (h *Handler) write(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(Response{
		Status:  "ok",
		Service: h.service,
		Version: h.version,
	})
}
