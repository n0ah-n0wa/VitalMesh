package handler

import (
	"log/slog"
	"net/http"

	"github.com/n0ah-n0wa/VitalMesh/services/api-gateway/internal/auth"
	"github.com/n0ah-n0wa/VitalMesh/services/api-gateway/internal/domain"
	"github.com/n0ah-n0wa/VitalMesh/services/api-gateway/internal/httpapi/model"
	"github.com/n0ah-n0wa/VitalMesh/services/api-gateway/internal/httpapi/request"
	"github.com/n0ah-n0wa/VitalMesh/services/api-gateway/internal/httpapi/respond"
	"github.com/n0ah-n0wa/VitalMesh/services/api-gateway/internal/pagination"
	"github.com/n0ah-n0wa/VitalMesh/services/api-gateway/internal/patient"
	"github.com/n0ah-n0wa/VitalMesh/services/api-gateway/internal/processing"
)

// Processing serves the Processing API (SPECIFICATIONS.md section 13).
// Every route is guarded by the authentication and authorization
// middleware.
type Processing struct {
	service *processing.Service
	logger  *slog.Logger
}

// NewProcessing returns a Processing handler backed by service.
func NewProcessing(service *processing.Service, logger *slog.Logger) *Processing {
	return &Processing{service: service, logger: logger}
}

// CreateJob handles POST /processing/jobs. The job is created, dispatched
// to the processor and answered in one request, so the response carries the
// job in its terminal state.
//
// A job that could not be processed for a transient reason answers 5xx,
// which leaves no idempotency record: the same Idempotency-Key can be used
// again to retry the whole request. The job row remains, FAILED, with the
// reason.
func (h *Processing) CreateJob(w http.ResponseWriter, r *http.Request) {
	actor, ok := auth.FromContext(r.Context())
	if !ok {
		respond.Error(w, r, h.logger, auth.AuthenticationRequired())
		return
	}
	var in model.CreateProcessingJobRequest
	if err := request.DecodeJSON(r, &in); err != nil {
		respond.Error(w, r, h.logger, err)
		return
	}
	job, err := h.service.Create(r.Context(), actor, processing.Input{
		PatientID:        in.PatientID,
		MeasurementTypes: in.MeasurementTypes,
		Windows:          in.Windows,
		Percentiles:      in.Percentiles,
		From:             in.From,
		To:               in.To,
	})
	if err != nil {
		respond.Error(w, r, h.logger, err)
		return
	}
	w.Header().Set("Location", r.URL.Path+"/"+job.ID.String())
	if err := respond.JSON(w, http.StatusCreated, processingJobModel(job)); err != nil {
		respond.Error(w, r, h.logger, err)
	}
}

// GetJob handles GET /processing/jobs/{job_id}.
func (h *Processing) GetJob(w http.ResponseWriter, r *http.Request) {
	actor, ok := auth.FromContext(r.Context())
	if !ok {
		respond.Error(w, r, h.logger, auth.AuthenticationRequired())
		return
	}
	id, ok := pathID(r, "job_id")
	if !ok {
		respond.Error(w, r, h.logger, processing.ErrJobNotFound())
		return
	}
	job, err := h.service.Get(r.Context(), actor, id)
	if err != nil {
		respond.Error(w, r, h.logger, err)
		return
	}
	if err := respond.JSON(w, http.StatusOK, processingJobModel(job)); err != nil {
		respond.Error(w, r, h.logger, err)
	}
}

// ListResults handles GET /patients/{patient_id}/processing-results.
func (h *Processing) ListResults(w http.ResponseWriter, r *http.Request) {
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
	var after *processing.ResultCursor
	if page.Cursor != "" {
		var cursor processing.ResultCursor
		if err := pagination.DecodeCursor(page.Cursor, &cursor); err != nil {
			respond.Error(w, r, h.logger, err)
			return
		}
		after = &cursor
	}
	result, err := h.service.Results(r.Context(), actor, patientID, after, page.Limit)
	if err != nil {
		respond.Error(w, r, h.logger, err)
		return
	}
	items := make([]model.ProcessingResult, len(result.Items))
	for i, res := range result.Items {
		items[i] = processingResultModel(res)
	}
	if err := respond.JSON(w, http.StatusOK, model.NewPage(items, result.NextCursor)); err != nil {
		respond.Error(w, r, h.logger, err)
	}
}

func processingJobModel(j domain.ProcessingJob) model.ProcessingJob {
	return model.ProcessingJob{
		ID:               j.ID,
		PatientID:        j.PatientID,
		Status:           string(j.Status),
		Parameters:       j.Parameters,
		AlgorithmVersion: j.AlgorithmVersion,
		ServiceVersion:   j.ServiceVersion,
		RequestedAt:      j.RequestedAt,
		StartedAt:        j.StartedAt,
		CompletedAt:      j.CompletedAt,
		FailedAt:         j.FailedAt,
		CancelledAt:      j.CancelledAt,
		ErrorCode:        j.ErrorCode,
		ErrorMessage:     j.ErrorMessage,
		AttemptCount:     j.AttemptCount,
		CreatedBy:        j.CreatedBy,
		CreatedAt:        j.CreatedAt,
	}
}

func processingResultModel(r domain.ProcessingResult) model.ProcessingResult {
	return model.ProcessingResult{
		ID:               r.ID,
		JobID:            r.JobID,
		PatientID:        r.PatientID,
		MeasurementType:  string(r.MeasurementType),
		Window:           r.Window,
		WindowStart:      r.WindowStart,
		Statistics:       r.Statistics,
		Anomalies:        r.Anomalies,
		AlgorithmVersion: r.AlgorithmVersion,
		ServiceVersion:   r.ServiceVersion,
		CreatedAt:        r.CreatedAt,
	}
}
