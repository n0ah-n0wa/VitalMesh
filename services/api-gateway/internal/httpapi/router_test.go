package httpapi

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/n0ah-n0wa/VitalMesh/services/api-gateway/internal/httpapi/model"
)

func newTestRouter() *Router {
	rt := NewRouter(slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil)))
	rt.HandleFunc(http.MethodGet, "/things", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("list"))
	})
	rt.HandleFunc(http.MethodPost, "/things", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusCreated)
	})
	return rt
}

func do(rt *Router, method, path string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	rt.ServeHTTP(rec, httptest.NewRequest(method, path, nil))
	return rec
}

func errorCode(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Fatalf("Content-Type = %q, want application/json (body: %s)", ct, rec.Body.String())
	}
	var body model.ErrorResponse
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return body.Error.Code
}

func TestRegisteredRoutes(t *testing.T) {
	rt := newTestRouter()

	if rec := do(rt, http.MethodGet, "/things"); rec.Code != http.StatusOK || rec.Body.String() != "list" {
		t.Errorf("GET /things: %d %q", rec.Code, rec.Body.String())
	}
	if rec := do(rt, http.MethodPost, "/things"); rec.Code != http.StatusCreated {
		t.Errorf("POST /things: %d", rec.Code)
	}
	if rec := do(rt, http.MethodHead, "/things"); rec.Code != http.StatusOK {
		t.Errorf("HEAD /things: %d, want 200 (GET routes serve HEAD)", rec.Code)
	}
}

func TestGroupPrefixesRoutes(t *testing.T) {
	rt := NewRouter(slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil)))
	v1 := rt.Group("/api/v1/")
	v1.HandleFunc(http.MethodGet, "/items", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("v1 items"))
	})

	if rec := do(rt, http.MethodGet, "/api/v1/items"); rec.Code != http.StatusOK || rec.Body.String() != "v1 items" {
		t.Errorf("GET /api/v1/items: %d %q", rec.Code, rec.Body.String())
	}
	if rec := do(rt, http.MethodGet, "/items"); rec.Code != http.StatusNotFound {
		t.Errorf("GET /items: %d, want 404 (route must only exist under the prefix)", rec.Code)
	}
	if rec := do(rt, http.MethodDelete, "/api/v1/items"); rec.Code != http.StatusMethodNotAllowed || rec.Header().Get("Allow") != "GET" {
		t.Errorf("DELETE /api/v1/items: %d Allow=%q", rec.Code, rec.Header().Get("Allow"))
	}
}

func TestUnknownPathIsJSON404(t *testing.T) {
	rec := do(newTestRouter(), http.MethodGet, "/nope")
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", rec.Code)
	}
	if code := errorCode(t, rec); code != "NOT_FOUND" {
		t.Errorf("code = %q, want NOT_FOUND", code)
	}
}

func TestWrongMethodIsJSON405WithAllow(t *testing.T) {
	rec := do(newTestRouter(), http.MethodDelete, "/things")
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("status = %d, want 405", rec.Code)
	}
	if allow := rec.Header().Get("Allow"); allow != "GET, POST" {
		t.Errorf("Allow = %q, want \"GET, POST\"", allow)
	}
	if code := errorCode(t, rec); code != "METHOD_NOT_ALLOWED" {
		t.Errorf("code = %q, want METHOD_NOT_ALLOWED", code)
	}
}

func TestLiteralAndWildcardSiblingsCoexist(t *testing.T) {
	// "/x/batch" and "/x/{id}" under different methods must both register
	// and each answer 405 for the methods it lacks, with the literal path
	// winning for its own method.
	rt := NewRouter(slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil)))
	rt.HandleFunc(http.MethodPost, "/api/v1/measurements/batch", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("batch")) })
	rt.HandleFunc(http.MethodGet, "/api/v1/measurements/{id}", func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte("one " + r.PathValue("id"))) })
	rt.HandleFunc(http.MethodDelete, "/api/v1/measurements/{id}", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })

	cases := []struct {
		method, path string
		status       int
		body, allow  string
	}{
		{http.MethodPost, "/api/v1/measurements/batch", http.StatusOK, "batch", ""},
		// For GET no literal "batch" route exists, so the wildcard applies.
		{http.MethodGet, "/api/v1/measurements/batch", http.StatusOK, "one batch", ""},
		{http.MethodPut, "/api/v1/measurements/batch", http.StatusMethodNotAllowed, "", "POST"},
		{http.MethodGet, "/api/v1/measurements/abc", http.StatusOK, "one abc", ""},
		{http.MethodDelete, "/api/v1/measurements/abc", http.StatusNoContent, "", ""},
		{http.MethodPut, "/api/v1/measurements/abc", http.StatusMethodNotAllowed, "", "GET, DELETE"},
		{http.MethodGet, "/api/v1/measurements/abc/extra", http.StatusNotFound, "", ""},
		{http.MethodGet, "/api/v1/measurements", http.StatusNotFound, "", ""},
	}
	for _, tc := range cases {
		rec := httptest.NewRecorder()
		rt.ServeHTTP(rec, httptest.NewRequest(tc.method, tc.path, nil))
		if rec.Code != tc.status {
			t.Errorf("%s %s: status = %d, want %d (%s)", tc.method, tc.path, rec.Code, tc.status, rec.Body.String())
		}
		if tc.body != "" && rec.Body.String() != tc.body {
			t.Errorf("%s %s: body = %q, want %q", tc.method, tc.path, rec.Body.String(), tc.body)
		}
		if got := rec.Header().Get("Allow"); got != tc.allow {
			t.Errorf("%s %s: Allow = %q, want %q", tc.method, tc.path, got, tc.allow)
		}
		if tc.status >= 400 && rec.Header().Get("Content-Type") != "application/json" {
			t.Errorf("%s %s: error is not the JSON envelope", tc.method, tc.path)
		}
	}
}
