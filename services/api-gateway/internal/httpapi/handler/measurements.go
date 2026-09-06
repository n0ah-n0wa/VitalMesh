package handler

import (
	"log/slog"
	"net/http"

	"github.com/google/uuid"

	"github.com/n0ah-n0wa/VitalMesh/services/api-gateway/internal/auth"
	"github.com/n0ah-n0wa/VitalMesh/services/api-gateway/internal/domain"
	"github.com/n0ah-n0wa/VitalMesh/services/api-gateway/internal/httpapi/model"
	"github.com/n0ah-n0wa/VitalMesh/services/api-gateway/internal/httpapi/request"
	"github.com/n0ah-n0wa/VitalMesh/services/api-gateway/internal/httpapi/respond"
	"github.com/n0ah-n0wa/VitalMesh/services/api-gateway/internal/measurement"
	"github.com/n0ah-n0wa/VitalMesh/services/api-gateway/internal/pagination"
	"github.com/n0ah-n0wa/VitalMesh/services/api-gateway/internal/patient"
)

// Measurements serves the Measurement API. Every route is guarded by the
// authentication and authorization middleware.
type Measurements struct {
	service *measurement.Service
	logger  *slog.Logger
}

// NewMeasurements returns a Measurements handler backed by service.
func NewMeasurements(service *measurement.Service, logger *slog.Logger) *Measurements {
	return &Measurements{service: service, logger: logger}
}

// Create handles POST /measurements.
func (h *Measurements) Create(w http.ResponseWriter, r *http.Request) {
	actor, ok := auth.FromContext(r.Context())
	if !ok {
		respond.Error(w, r, h.logger, auth.AuthenticationRequired())
		return
	}
	var in model.CreateMeasurementRequest
	if err := request.DecodeJSON(r, &in); err != nil {
		respond.Error(w, r, h.logger, err)
		return
	}
	created, err := h.service.Create(r.Context(), actor, toInput(in))
	if err != nil {
		respond.Error(w, r, h.logger, err)
		return
	}
	w.Header().Set("Location", r.URL.Path+"/"+created.ID.String())
	if err := respond.JSON(w, http.StatusCreated, measurementModel(created)); err != nil {
		respond.Error(w, r, h.logger, err)
	}
}

// CreateBatch handles POST /measurements/batch.
func (h *Measurements) CreateBatch(w http.ResponseWriter, r *http.Request) {
	actor, ok := auth.FromContext(r.Context())
	if !ok {
		respond.Error(w, r, h.logger, auth.AuthenticationRequired())
		return
	}
	var in model.CreateMeasurementBatchRequest
	if err := request.DecodeJSON(r, &in); err != nil {
		respond.Error(w, r, h.logger, err)
		return
	}
	inputs := make([]measurement.Input, len(in.Items))
	for i, item := range in.Items {
		inputs[i] = toInput(item)
	}
	created, err := h.service.CreateBatch(r.Context(), actor, inputs)
	if err != nil {
		respond.Error(w, r, h.logger, err)
		return
	}
	items := make([]model.Measurement, len(created))
	for i, m := range created {
		items[i] = measurementModel(m)
	}
	if err := respond.JSON(w, http.StatusCreated, model.MeasurementBatch{Items: items}); err != nil {
		respond.Error(w, r, h.logger, err)
	}
}

// Get handles GET /measurements/{measurement_id}.
func (h *Measurements) Get(w http.ResponseWriter, r *http.Request) {
	actor, ok := auth.FromContext(r.Context())
	if !ok {
		respond.Error(w, r, h.logger, auth.AuthenticationRequired())
		return
	}
	id, ok := pathID(r, "measurement_id")
	if !ok {
		respond.Error(w, r, h.logger, measurement.ErrNotFound())
		return
	}
	found, err := h.service.Get(r.Context(), actor, id)
	if err != nil {
		respond.Error(w, r, h.logger, err)
		return
	}
	if err := respond.JSON(w, http.StatusOK, measurementModel(found)); err != nil {
		respond.Error(w, r, h.logger, err)
	}
}

// ListByPatient handles GET /patients/{patient_id}/measurements with the
// optional type, from and to filters plus pagination.
func (h *Measurements) ListByPatient(w http.ResponseWriter, r *http.Request) {
	actor, ok := auth.FromContext(r.Context())
	if !ok {
		respond.Error(w, r, h.logger, auth.AuthenticationRequired())
		return
	}
	patientID, ok := pathID(r, "patient_id")
	if !ok {
		respond.Error(w, r, h.logger, patient.ErrNotFound())
		return
	}
	page, err := request.Pagination(r, pagination.StandardLimits())
	if err != nil {
		respond.Error(w, r, h.logger, err)
		return
	}
	q := r.URL.Query()
	result, err := h.service.ListByPatient(r.Context(), actor, patientID, measurement.ListInput{
		Type: q.Get("type"), From: q.Get("from"), To: q.Get("to"), Page: page,
	})
	if err != nil {
		respond.Error(w, r, h.logger, err)
		return
	}
	items := make([]model.Measurement, len(result.Items))
	for i, m := range result.Items {
		items[i] = measurementModel(m)
	}
	if err := respond.JSON(w, http.StatusOK, model.NewPage(items, result.NextCursor)); err != nil {
		respond.Error(w, r, h.logger, err)
	}
}

// Delete handles DELETE /measurements/{measurement_id}.
func (h *Measurements) Delete(w http.ResponseWriter, r *http.Request) {
	actor, ok := auth.FromContext(r.Context())
	if !ok {
		respond.Error(w, r, h.logger, auth.AuthenticationRequired())
		return
	}
	id, ok := pathID(r, "measurement_id")
	if !ok {
		respond.Error(w, r, h.logger, measurement.ErrNotFound())
		return
	}
	if err := h.service.Delete(r.Context(), actor, id); err != nil {
		respond.Error(w, r, h.logger, err)
		return
	}
	respond.NoContent(w)
}

// pathID reads a UUID path parameter. A value that is not a UUID cannot
// name a resource, so callers answer not-found without revealing more.
func pathID(r *http.Request, name string) (uuid.UUID, bool) {
	id, err := uuid.Parse(r.PathValue(name))
	if err != nil || id == uuid.Nil {
		return uuid.Nil, false
	}
	return id, true
}

func toInput(in model.CreateMeasurementRequest) measurement.Input {
	return measurement.Input{
		PatientID: in.PatientID, Type: in.Type, Value: in.Value, Unit: in.Unit,
		RecordedAt: in.RecordedAt, Source: in.Source, Metadata: in.Metadata,
	}
}

func measurementModel(m domain.Measurement) model.Measurement {
	metadata := m.Metadata
	if len(metadata) == 0 {
		metadata = []byte(`{}`)
	}
	return model.Measurement{
		ID: m.ID, PatientID: m.PatientID, Type: string(m.Type), Value: m.Value, Unit: m.Unit,
		RecordedAt: m.RecordedAt, CreatedAt: m.CreatedAt, Source: m.Source, Metadata: metadata,
	}
}
