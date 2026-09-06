package respond

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/n0ah-n0wa/VitalMesh/services/api-gateway/internal/domain"
	"github.com/n0ah-n0wa/VitalMesh/services/api-gateway/internal/httpapi/model"
	"github.com/n0ah-n0wa/VitalMesh/services/api-gateway/internal/requestid"
)

func decodeError(t *testing.T, rec *httptest.ResponseRecorder) model.ErrorResponse {
	t.Helper()
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Fatalf("Content-Type = %q, want application/json", ct)
	}
	var body model.ErrorResponse
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return body
}

func request() *http.Request {
	r := httptest.NewRequest(http.MethodGet, "/x", nil)
	return r.WithContext(requestid.NewContext(r.Context(), "req-9"))
}

func TestJSON(t *testing.T) {
	rec := httptest.NewRecorder()
	if err := JSON(rec, http.StatusCreated, map[string]int{"n": 1}); err != nil {
		t.Fatalf("JSON: %v", err)
	}

	if rec.Code != http.StatusCreated {
		t.Errorf("status = %d, want 201", rec.Code)
	}
	if got := rec.Body.String(); got != `{"n":1}` {
		t.Errorf("body = %q", got)
	}
	if got := rec.Header().Get("Content-Length"); got != "7" {
		t.Errorf("Content-Length = %q, want 7", got)
	}
}

func TestJSONReportsEncodingFailureBeforeWriting(t *testing.T) {
	rec := httptest.NewRecorder()
	err := JSON(rec, http.StatusOK, map[string]any{"bad": make(chan int)})

	if err == nil {
		t.Fatal("JSON returned nil for an unencodable body")
	}
	if rec.Body.Len() != 0 || len(rec.Header()) != 0 {
		t.Errorf("response was partially written: status=%d body=%q headers=%v", rec.Code, rec.Body.String(), rec.Header())
	}
}

func TestErrorMapsDomainKinds(t *testing.T) {
	cases := []struct {
		kind domain.Kind
		want int
	}{
		{domain.KindInvalid, http.StatusBadRequest},
		{domain.KindValidation, http.StatusUnprocessableEntity},
		{domain.KindNotFound, http.StatusNotFound},
		{domain.KindConflict, http.StatusConflict},
		{domain.KindUnauthorized, http.StatusUnauthorized},
		{domain.KindForbidden, http.StatusForbidden},
		{domain.KindRateLimited, http.StatusTooManyRequests},
		{domain.KindTooLarge, http.StatusRequestEntityTooLarge},
		{domain.KindUnsupportedMedia, http.StatusUnsupportedMediaType},
		{domain.KindUnavailable, http.StatusServiceUnavailable},
		{domain.KindTimeout, http.StatusGatewayTimeout},
	}
	logger := slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil))

	for _, tc := range cases {
		t.Run(tc.kind.String(), func(t *testing.T) {
			rec := httptest.NewRecorder()
			err := fmt.Errorf("wrapped: %w", domain.New(tc.kind, "SOME_CODE", "Some message."))
			Error(rec, request(), logger, err)

			if rec.Code != tc.want {
				t.Errorf("status = %d, want %d", rec.Code, tc.want)
			}
			body := decodeError(t, rec)
			if body.Error.Code != "SOME_CODE" || body.Error.Message != "Some message." || body.Error.RequestID != "req-9" || body.Error.Details != nil {
				t.Errorf("body = %+v", body.Error)
			}
		})
	}
}

func TestErrorHidesInternalCauses(t *testing.T) {
	cases := map[string]error{
		"plain error":         errors.New("pq: password authentication failed for user admin"),
		"internal domain err": domain.Wrap(errors.New("dial tcp 10.0.0.5:5432: refused"), domain.KindInternal, "DB_WRITE", "Write failed."),
	}

	for name, err := range cases {
		t.Run(name, func(t *testing.T) {
			var logBuf bytes.Buffer
			logger := slog.New(slog.NewTextHandler(&logBuf, nil))
			rec := httptest.NewRecorder()

			Error(rec, request(), logger, err)

			if rec.Code != http.StatusInternalServerError {
				t.Errorf("status = %d, want 500", rec.Code)
			}
			body := decodeError(t, rec)
			if body.Error.Code != InternalCode || body.Error.Message != InternalMessage {
				t.Errorf("body = %+v, want generic internal error", body.Error)
			}
			for _, leak := range []string{"password", "10.0.0.5", "DB_WRITE", "Write failed"} {
				if strings.Contains(rec.Body.String(), leak) {
					t.Errorf("response leaks %q: %s", leak, rec.Body.String())
				}
			}
			if !strings.Contains(logBuf.String(), "request failed") {
				t.Errorf("internal error was not logged: %s", logBuf.String())
			}
		})
	}
}

func TestErrorIncludesValidationDetails(t *testing.T) {
	rec := httptest.NewRecorder()
	err := &domain.Error{
		Kind: domain.KindValidation, Code: "VALIDATION_FAILED", Message: "The request is invalid.",
		Details: []domain.FieldError{{Field: "name", Message: "is required"}},
	}
	Error(rec, request(), slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil)), err)

	if rec.Code != http.StatusUnprocessableEntity {
		t.Errorf("status = %d, want 422", rec.Code)
	}
	body := decodeError(t, rec)
	if len(body.Error.Details) != 1 || body.Error.Details[0] != (model.FieldError{Field: "name", Message: "is required"}) {
		t.Errorf("details = %+v", body.Error.Details)
	}
}

func TestErrorMapsContextErrors(t *testing.T) {
	var logBuf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logBuf, nil))
	cases := []struct {
		err    error
		status int
		code   string
	}{
		{fmt.Errorf("query: %w", context.DeadlineExceeded), http.StatusGatewayTimeout, TimeoutCode},
		{context.Canceled, http.StatusServiceUnavailable, CancelledCode},
	}
	for _, tc := range cases {
		rec := httptest.NewRecorder()
		Error(rec, request(), logger, tc.err)
		if rec.Code != tc.status {
			t.Errorf("%v: status = %d, want %d", tc.err, rec.Code, tc.status)
		}
		if body := decodeError(t, rec); body.Error.Code != tc.code {
			t.Errorf("%v: code = %q, want %q", tc.err, body.Error.Code, tc.code)
		}
	}
	if logBuf.Len() != 0 {
		t.Errorf("context errors were logged as failures: %s", logBuf.String())
	}
}

func TestNoContent(t *testing.T) {
	rec := httptest.NewRecorder()
	NoContent(rec)
	if rec.Code != http.StatusNoContent || rec.Body.Len() != 0 {
		t.Errorf("status = %d body = %q", rec.Code, rec.Body.String())
	}
}

func TestErrorStatus(t *testing.T) {
	rec := httptest.NewRecorder()
	ErrorStatus(rec, request(), http.StatusMethodNotAllowed, "METHOD_NOT_ALLOWED", "Nope.")

	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("status = %d, want 405", rec.Code)
	}
	if body := decodeError(t, rec); body.Error.Code != "METHOD_NOT_ALLOWED" || body.Error.RequestID != "req-9" {
		t.Errorf("body = %+v", body.Error)
	}
}
