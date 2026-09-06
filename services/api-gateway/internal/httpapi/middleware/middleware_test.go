package middleware

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/n0ah-n0wa/VitalMesh/services/api-gateway/internal/config"
	"github.com/n0ah-n0wa/VitalMesh/services/api-gateway/internal/httpapi/model"
	"github.com/n0ah-n0wa/VitalMesh/services/api-gateway/internal/observability/logging"
	"github.com/n0ah-n0wa/VitalMesh/services/api-gateway/internal/requestid"
)

func testLogger() (*slog.Logger, *bytes.Buffer) {
	var buf bytes.Buffer
	return logging.New(&buf, config.Log{Format: config.LogFormatJSON}, logging.Service{}), &buf
}

func lastLogLine(t *testing.T, buf *bytes.Buffer) map[string]any {
	t.Helper()
	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	var rec map[string]any
	if err := json.Unmarshal([]byte(lines[len(lines)-1]), &rec); err != nil {
		t.Fatalf("log line is not JSON: %v\n%s", err, buf.String())
	}
	return rec
}

func TestChainOrder(t *testing.T) {
	var order []string
	tag := func(name string) Middleware {
		return func(next http.Handler) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				order = append(order, name+":in")
				next.ServeHTTP(w, r)
				order = append(order, name+":out")
			})
		}
	}
	h := Chain(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		order = append(order, "handler")
	}), tag("a"), tag("b"))

	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil))

	want := "a:in b:in handler b:out a:out"
	if got := strings.Join(order, " "); got != want {
		t.Errorf("order = %q, want %q", got, want)
	}
}

func TestRequestID(t *testing.T) {
	cases := []struct {
		name   string
		header string
		keep   bool
	}{
		{"generated when absent", "", false},
		{"client value kept", "client-abc.1", true},
		{"invalid value replaced", "bad value!", false},
		{"oversized value replaced", strings.Repeat("x", 129), false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var seen string
			h := Chain(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				seen = requestid.FromContext(r.Context())
			}), RequestID())

			req := httptest.NewRequest(http.MethodGet, "/", nil)
			if tc.header != "" {
				req.Header.Set(requestid.Header, tc.header)
			}
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)

			echoed := rec.Header().Get(requestid.Header)
			if seen == "" || echoed != seen {
				t.Fatalf("context id %q, response header %q", seen, echoed)
			}
			if !requestid.Valid(seen) {
				t.Errorf("id %q is not valid", seen)
			}
			if tc.keep && seen != tc.header {
				t.Errorf("valid client id %q was replaced by %q", tc.header, seen)
			}
			if !tc.keep && seen == tc.header {
				t.Errorf("invalid client id %q was kept", tc.header)
			}
		})
	}
}

func TestLoggingRecordsRequestOutcome(t *testing.T) {
	logger, buf := testLogger()
	h := Chain(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTeapot)
		_, _ = w.Write([]byte("short and stout"))
	}), RequestID(), Logging(logger))

	req := httptest.NewRequest(http.MethodPost, "/teapots?x=1", nil)
	req.Header.Set(requestid.Header, "req-log")
	h.ServeHTTP(httptest.NewRecorder(), req)

	rec := lastLogLine(t, buf)
	for key, want := range map[string]any{
		"message":    "request",
		"method":     "POST",
		"path":       "/teapots",
		"status":     float64(http.StatusTeapot),
		"bytes":      float64(len("short and stout")),
		"request_id": "req-log",
	} {
		if rec[key] != want {
			t.Errorf("%s = %v, want %v", key, rec[key], want)
		}
	}
	if _, ok := rec["duration_ms"]; !ok {
		t.Error("duration_ms missing")
	}
}

func TestLoggingDefaultsStatusTo200(t *testing.T) {
	logger, buf := testLogger()
	h := Chain(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("ok"))
	}), Logging(logger))

	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil))

	if got := lastLogLine(t, buf)["status"]; got != float64(http.StatusOK) {
		t.Errorf("status = %v, want 200", got)
	}
}

func TestRecoverReturnsGenericErrorAndLogsStack(t *testing.T) {
	logger, buf := testLogger()
	h := Chain(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		panic("nil pointer dereference at internal/secret.go:42")
	}), RequestID(), Recover(logger))

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))

	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", rec.Code)
	}
	var body model.ErrorResponse
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.Error.Code != "INTERNAL_ERROR" || body.Error.RequestID == "" {
		t.Errorf("body = %+v", body.Error)
	}
	if strings.Contains(rec.Body.String(), "secret.go") {
		t.Error("panic details leaked to the client")
	}

	entry := lastLogLine(t, buf)
	if entry["message"] != "panic recovered" || !strings.Contains(entry["stack"].(string), "goroutine") {
		t.Errorf("panic was not logged with a stack: %v", entry)
	}
}

func TestRecoverRethrowsAbortHandler(t *testing.T) {
	logger, _ := testLogger()
	h := Chain(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		panic(http.ErrAbortHandler)
	}), Recover(logger))

	defer func() {
		if p := recover(); p != http.ErrAbortHandler {
			t.Errorf("recovered %v, want http.ErrAbortHandler to propagate", p)
		}
	}()
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil))
}
