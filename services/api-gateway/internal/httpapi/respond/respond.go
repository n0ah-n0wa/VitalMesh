// Package respond writes HTTP responses: JSON bodies and the error envelope,
// including the mapping from domain error kinds to status codes.
package respond

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strconv"

	"github.com/n0ah-n0wa/VitalMesh/services/api-gateway/internal/domain"
	"github.com/n0ah-n0wa/VitalMesh/services/api-gateway/internal/httpapi/model"
	"github.com/n0ah-n0wa/VitalMesh/services/api-gateway/internal/requestid"
)

// Codes and messages for failures that have no domain counterpart.
const (
	InternalCode    = "INTERNAL_ERROR"
	InternalMessage = "An internal error occurred."

	TimeoutCode    = "REQUEST_TIMEOUT"
	TimeoutMessage = "The request took too long to complete."

	CancelledCode    = "REQUEST_CANCELLED"
	CancelledMessage = "The request was cancelled before it completed."
)

// JSON writes body as JSON with the given status. The body is encoded before
// anything is sent, so an encoding failure (a programming error in a wire
// model) is returned with nothing written and the caller can still respond
// with an error. Failures while writing to the connection are not reported:
// the client is gone and the logging middleware records the outcome.
func JSON(w http.ResponseWriter, status int, body any) error {
	data, err := json.Marshal(body)
	if err != nil {
		return err
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Content-Length", strconv.Itoa(len(data)))
	w.WriteHeader(status)
	_, _ = w.Write(data)
	return nil
}

// NoContent writes a 204 response.
func NoContent(w http.ResponseWriter) {
	w.WriteHeader(http.StatusNoContent)
}

// Error writes the error envelope for err.
//
// A *domain.Error is mapped by its Kind and carries its Details. A context
// deadline or cancellation is reported as such. Anything else, including
// every KindInternal error, is reported as a generic internal error and
// logged with its cause so that no implementation detail reaches the client.
func Error(w http.ResponseWriter, r *http.Request, logger *slog.Logger, err error) {
	var domErr *domain.Error
	switch {
	case errors.As(err, &domErr) && domErr.Kind != domain.KindInternal:
		writeEnvelope(w, r, statusFor(domErr.Kind), domErr.Code, domErr.Message, domErr.Details)
	case errors.Is(err, context.DeadlineExceeded):
		ErrorStatus(w, r, http.StatusGatewayTimeout, TimeoutCode, TimeoutMessage)
	case errors.Is(err, context.Canceled):
		ErrorStatus(w, r, http.StatusServiceUnavailable, CancelledCode, CancelledMessage)
	default:
		logger.ErrorContext(r.Context(), "request failed", "error", err)
		ErrorStatus(w, r, http.StatusInternalServerError, InternalCode, InternalMessage)
	}
}

// ErrorStatus writes the error envelope with an explicit status. Use it for
// transport-level failures that have no domain counterpart, such as 405.
func ErrorStatus(w http.ResponseWriter, r *http.Request, status int, code, message string) {
	writeEnvelope(w, r, status, code, message, nil)
}

// BearerRealm is the protection space named in WWW-Authenticate challenges.
const BearerRealm = "vitalmesh"

func writeEnvelope(w http.ResponseWriter, r *http.Request, status int, code, message string, details []domain.FieldError) {
	// RFC 6750: every 401 carries a challenge. Callers that know more (an
	// invalid token, say) set a more specific one first.
	if status == http.StatusUnauthorized && w.Header().Get("WWW-Authenticate") == "" {
		w.Header().Set("WWW-Authenticate", `Bearer realm="`+BearerRealm+`"`)
	}
	envelope := model.ErrorResponse{Error: model.ErrorDetail{
		Code:      code,
		Message:   message,
		RequestID: requestid.FromContext(r.Context()),
		Details:   fieldErrors(details),
	}}
	if err := JSON(w, status, envelope); err != nil {
		// Unreachable for a struct of strings; kept so that no failure is
		// silently dropped.
		http.Error(w, InternalMessage, http.StatusInternalServerError)
	}
}

func fieldErrors(in []domain.FieldError) []model.FieldError {
	if len(in) == 0 {
		return nil
	}
	out := make([]model.FieldError, len(in))
	for i, fe := range in {
		out[i] = model.FieldError{Field: fe.Field, Message: fe.Message}
	}
	return out
}

func statusFor(kind domain.Kind) int {
	switch kind {
	case domain.KindInvalid:
		return http.StatusBadRequest
	case domain.KindValidation:
		return http.StatusUnprocessableEntity
	case domain.KindNotFound:
		return http.StatusNotFound
	case domain.KindConflict:
		return http.StatusConflict
	case domain.KindUnauthorized:
		return http.StatusUnauthorized
	case domain.KindForbidden:
		return http.StatusForbidden
	case domain.KindRateLimited:
		return http.StatusTooManyRequests
	case domain.KindTooLarge:
		return http.StatusRequestEntityTooLarge
	case domain.KindUnsupportedMedia:
		return http.StatusUnsupportedMediaType
	case domain.KindUnavailable:
		return http.StatusServiceUnavailable
	case domain.KindTimeout:
		return http.StatusGatewayTimeout
	default:
		return http.StatusInternalServerError
	}
}
