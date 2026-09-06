package handler

import (
	"log/slog"
	"net/http"
	"time"

	"github.com/google/uuid"

	"github.com/n0ah-n0wa/VitalMesh/services/api-gateway/internal/auth"
	"github.com/n0ah-n0wa/VitalMesh/services/api-gateway/internal/domain"
	"github.com/n0ah-n0wa/VitalMesh/services/api-gateway/internal/httpapi/model"
	"github.com/n0ah-n0wa/VitalMesh/services/api-gateway/internal/httpapi/request"
	"github.com/n0ah-n0wa/VitalMesh/services/api-gateway/internal/httpapi/respond"
	"github.com/n0ah-n0wa/VitalMesh/services/api-gateway/internal/pagination"
	"github.com/n0ah-n0wa/VitalMesh/services/api-gateway/internal/patient"
)

// Patients serves the Patient API. Every route is guarded by the
// authentication and authorization middleware; the handlers assume a
// principal is present and treat its absence as a programming error
// answered with 401.
type Patients struct {
	service *patient.Service
	logger  *slog.Logger
}

// NewPatients returns a Patients handler backed by service.
func NewPatients(service *patient.Service, logger *slog.Logger) *Patients {
	return &Patients{service: service, logger: logger}
}

// Create handles POST /patients.
func (h *Patients) Create(w http.ResponseWriter, r *http.Request) {
	actor, ok := auth.FromContext(r.Context())
	if !ok {
		respond.Error(w, r, h.logger, auth.AuthenticationRequired())
		return
	}
	var in model.CreatePatientRequest
	if err := request.DecodeJSON(r, &in); err != nil {
		respond.Error(w, r, h.logger, err)
		return
	}
	created, err := h.service.Create(r.Context(), actor, patient.CreateInput{
		ExternalReference: in.ExternalReference,
		DateOfBirth:       in.DateOfBirth,
		Sex:               in.Sex,
	})
	if err != nil {
		respond.Error(w, r, h.logger, err)
		return
	}
	w.Header().Set("Location", r.URL.Path+"/"+created.ID.String())
	if err := respond.JSON(w, http.StatusCreated, patientModel(created)); err != nil {
		respond.Error(w, r, h.logger, err)
	}
}

// Get handles GET /patients/{patient_id}.
func (h *Patients) Get(w http.ResponseWriter, r *http.Request) {
	actor, ok := auth.FromContext(r.Context())
	if !ok {
		respond.Error(w, r, h.logger, auth.AuthenticationRequired())
		return
	}
	id, ok := patientID(r)
	if !ok {
		respond.Error(w, r, h.logger, patient.ErrNotFound())
		return
	}
	found, err := h.service.Get(r.Context(), actor, id)
	if err != nil {
		respond.Error(w, r, h.logger, err)
		return
	}
	if err := respond.JSON(w, http.StatusOK, patientModel(found)); err != nil {
		respond.Error(w, r, h.logger, err)
	}
}

// List handles GET /patients.
func (h *Patients) List(w http.ResponseWriter, r *http.Request) {
	page, err := request.Pagination(r, pagination.StandardLimits())
	if err != nil {
		respond.Error(w, r, h.logger, err)
		return
	}
	result, err := h.service.List(r.Context(), page)
	if err != nil {
		respond.Error(w, r, h.logger, err)
		return
	}
	items := make([]model.Patient, len(result.Items))
	for i, p := range result.Items {
		items[i] = patientModel(p)
	}
	if err := respond.JSON(w, http.StatusOK, model.NewPage(items, result.NextCursor)); err != nil {
		respond.Error(w, r, h.logger, err)
	}
}

// Delete handles DELETE /patients/{patient_id}.
func (h *Patients) Delete(w http.ResponseWriter, r *http.Request) {
	actor, ok := auth.FromContext(r.Context())
	if !ok {
		respond.Error(w, r, h.logger, auth.AuthenticationRequired())
		return
	}
	id, ok := patientID(r)
	if !ok {
		respond.Error(w, r, h.logger, patient.ErrNotFound())
		return
	}
	if err := h.service.Delete(r.Context(), actor, id); err != nil {
		respond.Error(w, r, h.logger, err)
		return
	}
	respond.NoContent(w)
}

// patientID reads the path parameter. A value that is not a UUID cannot
// name a patient, so callers answer not-found without revealing more.
func patientID(r *http.Request) (uuid.UUID, bool) {
	return pathID(r, "patient_id")
}

func patientModel(p domain.Patient) model.Patient {
	return model.Patient{
		ID:                p.ID,
		ExternalReference: p.ExternalReference,
		DateOfBirth:       p.DateOfBirth.UTC().Format(time.DateOnly),
		Sex:               string(p.Sex),
		Status:            string(p.Status),
		CreatedAt:         p.CreatedAt,
		UpdatedAt:         p.UpdatedAt,
	}
}
