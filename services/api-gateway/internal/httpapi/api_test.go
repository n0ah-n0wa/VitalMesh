package httpapi

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/n0ah-n0wa/VitalMesh/services/api-gateway/internal/config"
	"github.com/n0ah-n0wa/VitalMesh/services/api-gateway/internal/httpapi/model"
	"github.com/n0ah-n0wa/VitalMesh/services/api-gateway/internal/httpapi/request"
	"github.com/n0ah-n0wa/VitalMesh/services/api-gateway/internal/httpapi/respond"
	"github.com/n0ah-n0wa/VitalMesh/services/api-gateway/internal/observability/logging"
	"github.com/n0ah-n0wa/VitalMesh/services/api-gateway/internal/pagination"
	"github.com/n0ah-n0wa/VitalMesh/services/api-gateway/internal/requestid"
	"github.com/n0ah-n0wa/VitalMesh/services/api-gateway/internal/validate"
)

// widget is a stand-in resource exercising the whole request pipeline the
// way a real feature handler will: decode, validate, respond.
type widget struct {
	Name string `json:"name"`
	Size int    `json:"size"`
}

const testBodyLimit = 64

// newAPI builds the full middleware chain around a router with test routes
// under /api/v1 and returns it with the captured log output.
func newAPI(t *testing.T) (http.Handler, *bytes.Buffer) {
	t.Helper()
	var logBuf bytes.Buffer
	logger := logging.New(&logBuf, config.Log{Format: config.LogFormatJSON}, logging.Service{Name: "test"})
	cfg := config.HTTP{MaxBodyBytes: testBodyLimit, RequestTimeout: 100 * time.Millisecond}

	rt := NewRouter(logger)
	v1 := rt.Group(APIv1)

	v1.HandleFunc(http.MethodPost, "/widgets", func(w http.ResponseWriter, r *http.Request) {
		var in widget
		if err := request.DecodeJSON(r, &in); err != nil {
			respond.Error(w, r, logger, err)
			return
		}
		var v validate.Validator
		v.Required("name", in.Name)
		validate.InRange(&v, "size", in.Size, 1, 10)
		if err := v.Err(); err != nil {
			respond.Error(w, r, logger, err)
			return
		}
		if err := respond.JSON(w, http.StatusCreated, in); err != nil {
			respond.Error(w, r, logger, err)
		}
	})
	v1.HandleFunc(http.MethodGet, "/widgets", func(w http.ResponseWriter, r *http.Request) {
		page, err := request.Pagination(r, pagination.StandardLimits())
		if err != nil {
			respond.Error(w, r, logger, err)
			return
		}
		items := []widget{{Name: "a", Size: 1}}
		next := ""
		if page.Limit == 1 {
			next = "opaque"
		}
		_ = respond.JSON(w, http.StatusOK, model.NewPage(items, next))
	})
	v1.HandleFunc(http.MethodGet, "/boom", func(w http.ResponseWriter, r *http.Request) {
		respond.Error(w, r, logger, errors.New("pq: connection to 10.0.0.9:5432 refused"))
	})
	v1.HandleFunc(http.MethodGet, "/panic", func(http.ResponseWriter, *http.Request) {
		panic("index out of range in internal/secret.go")
	})
	v1.HandleFunc(http.MethodGet, "/slow", func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-time.After(5 * time.Second):
		}
		w.WriteHeader(http.StatusOK)
	})

	return Wrap(cfg, logger, rt), &logBuf
}

func call(h http.Handler, method, path, body string, headers map[string]string) *httptest.ResponseRecorder {
	var rd io.Reader
	if body != "" {
		rd = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, path, rd)
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func envelope(t *testing.T, rec *httptest.ResponseRecorder) model.ErrorDetail {
	t.Helper()
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Fatalf("Content-Type = %q, body %q", ct, rec.Body.String())
	}
	var body model.ErrorResponse
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatalf("decode envelope: %v", err)
	}
	if body.Error.RequestID == "" {
		t.Error("envelope has no request_id")
	}
	return body.Error
}

func TestMalformedJSON(t *testing.T) {
	h, _ := newAPI(t)
	rec := call(h, http.MethodPost, "/api/v1/widgets", `{"name": "x", `, nil)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (%s)", rec.Code, rec.Body.String())
	}
	if e := envelope(t, rec); e.Code != request.CodeInvalidJSON {
		t.Errorf("code = %q", e.Code)
	}
}

func TestUnknownFieldIsRejected(t *testing.T) {
	h, _ := newAPI(t)
	rec := call(h, http.MethodPost, "/api/v1/widgets", `{"name":"x","size":2,"colour":"red"}`, nil)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
	if e := envelope(t, rec); len(e.Details) != 1 || e.Details[0].Field != "colour" {
		t.Errorf("details = %+v", e.Details)
	}
}

func TestOversizedRequests(t *testing.T) {
	h, _ := newAPI(t)
	big := `{"name":"` + strings.Repeat("x", testBodyLimit) + `"}`

	t.Run("declared length", func(t *testing.T) {
		rec := call(h, http.MethodPost, "/api/v1/widgets", big, nil)
		if rec.Code != http.StatusRequestEntityTooLarge {
			t.Fatalf("status = %d, want 413", rec.Code)
		}
		if e := envelope(t, rec); e.Code != request.CodeBodyTooLarge {
			t.Errorf("code = %q", e.Code)
		}
	})

	t.Run("undeclared length", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, "/api/v1/widgets", io.NopCloser(strings.NewReader(big)))
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusRequestEntityTooLarge {
			t.Fatalf("status = %d, want 413", rec.Code)
		}
		if e := envelope(t, rec); e.Code != request.CodeBodyTooLarge {
			t.Errorf("code = %q", e.Code)
		}
	})
}

func TestUnsupportedMediaType(t *testing.T) {
	h, _ := newAPI(t)
	rec := call(h, http.MethodPost, "/api/v1/widgets", `{"name":"x","size":1}`, map[string]string{"Content-Type": "text/plain"})
	if rec.Code != http.StatusUnsupportedMediaType {
		t.Fatalf("status = %d, want 415", rec.Code)
	}
	envelope(t, rec)
}

func TestValidationFailure(t *testing.T) {
	h, _ := newAPI(t)
	rec := call(h, http.MethodPost, "/api/v1/widgets", `{"name":"  ","size":99}`, nil)

	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422", rec.Code)
	}
	e := envelope(t, rec)
	if e.Code != validate.Code {
		t.Errorf("code = %q", e.Code)
	}
	fields := map[string]string{}
	for _, d := range e.Details {
		fields[d.Field] = d.Message
	}
	if fields["name"] != "is required" || !strings.HasPrefix(fields["size"], "must be between") {
		t.Errorf("details = %+v", e.Details)
	}
}

func TestValidRequestSucceeds(t *testing.T) {
	h, _ := newAPI(t)
	rec := call(h, http.MethodPost, "/api/v1/widgets", `{"name":"x","size":3}`, nil)
	if rec.Code != http.StatusCreated || rec.Body.String() != `{"name":"x","size":3}` {
		t.Errorf("got %d %q", rec.Code, rec.Body.String())
	}
}

func TestPaginationPipeline(t *testing.T) {
	h, _ := newAPI(t)

	rec := call(h, http.MethodGet, "/api/v1/widgets?limit=1", "", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d (%s)", rec.Code, rec.Body.String())
	}
	if want := `{"items":[{"name":"a","size":1}],"next_cursor":"opaque","has_more":true}`; rec.Body.String() != want {
		t.Errorf("body = %s", rec.Body.String())
	}

	rec = call(h, http.MethodGet, "/api/v1/widgets?limit=abc", "", nil)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422", rec.Code)
	}
	if e := envelope(t, rec); len(e.Details) != 1 || e.Details[0].Field != "limit" {
		t.Errorf("details = %+v", e.Details)
	}
}

func TestUnknownRoutes(t *testing.T) {
	h, _ := newAPI(t)
	for _, path := range []string{"/api/v1/nope", "/api/v2/widgets", "/widgets"} {
		rec := call(h, http.MethodGet, path, "", nil)
		if rec.Code != http.StatusNotFound {
			t.Errorf("GET %s: status = %d, want 404", path, rec.Code)
			continue
		}
		if e := envelope(t, rec); e.Code != "NOT_FOUND" {
			t.Errorf("GET %s: code = %q", path, e.Code)
		}
		if rec.Header().Get("X-Content-Type-Options") != "nosniff" {
			t.Errorf("GET %s: security headers missing on error responses", path)
		}
	}

	rec := call(h, http.MethodDelete, "/api/v1/widgets", "", nil)
	if rec.Code != http.StatusMethodNotAllowed || rec.Header().Get("Allow") != "POST, GET" {
		t.Errorf("DELETE: %d Allow=%q", rec.Code, rec.Header().Get("Allow"))
	}
}

func TestInternalErrorsAreHidden(t *testing.T) {
	h, logBuf := newAPI(t)

	for _, path := range []string{"/api/v1/boom", "/api/v1/panic"} {
		logBuf.Reset()
		rec := call(h, http.MethodGet, path, "", map[string]string{requestid.Header: "trace-me"})

		if rec.Code != http.StatusInternalServerError {
			t.Errorf("GET %s: status = %d, want 500", path, rec.Code)
		}
		e := envelope(t, rec)
		if e.Code != respond.InternalCode || e.Message != respond.InternalMessage || e.RequestID != "trace-me" {
			t.Errorf("GET %s: envelope = %+v", path, e)
		}
		for _, leak := range []string{"10.0.0.9", "secret.go", "goroutine", "pq:"} {
			if strings.Contains(rec.Body.String(), leak) {
				t.Errorf("GET %s: response leaks %q", path, leak)
			}
		}
		if !strings.Contains(logBuf.String(), `"request_id":"trace-me"`) || !strings.Contains(logBuf.String(), `"level":"ERROR"`) {
			t.Errorf("GET %s: failure not logged with request id: %s", path, logBuf.String())
		}
	}
}

func TestRequestIDPropagation(t *testing.T) {
	h, logBuf := newAPI(t)

	t.Run("client supplied", func(t *testing.T) {
		logBuf.Reset()
		rec := call(h, http.MethodGet, "/api/v1/missing", "", map[string]string{requestid.Header: "client-7"})
		if got := rec.Header().Get(requestid.Header); got != "client-7" {
			t.Errorf("response header = %q", got)
		}
		if e := envelope(t, rec); e.RequestID != "client-7" {
			t.Errorf("envelope request_id = %q", e.RequestID)
		}
		if !strings.Contains(logBuf.String(), `"request_id":"client-7"`) {
			t.Errorf("log line lacks request id: %s", logBuf.String())
		}
	})

	t.Run("generated", func(t *testing.T) {
		rec := call(h, http.MethodGet, "/api/v1/missing", "", nil)
		id := rec.Header().Get(requestid.Header)
		if !requestid.Valid(id) {
			t.Fatalf("generated id %q invalid", id)
		}
		if e := envelope(t, rec); e.RequestID != id {
			t.Errorf("envelope request_id = %q, header %q", e.RequestID, id)
		}
	})
}

func TestRequestTimeout(t *testing.T) {
	h, _ := newAPI(t)
	rec := call(h, http.MethodGet, "/api/v1/slow", "", nil)
	if rec.Code != http.StatusGatewayTimeout {
		t.Fatalf("status = %d, want 504", rec.Code)
	}
	if e := envelope(t, rec); e.Code != respond.TimeoutCode {
		t.Errorf("code = %q", e.Code)
	}
}

func TestSecurityHeadersOnSuccess(t *testing.T) {
	h, _ := newAPI(t)
	rec := call(h, http.MethodPost, "/api/v1/widgets", `{"name":"x","size":1}`, nil)
	if rec.Header().Get("Cache-Control") != "no-store" || rec.Header().Get("X-Frame-Options") != "DENY" {
		t.Errorf("security headers missing: %v", rec.Header())
	}
}
