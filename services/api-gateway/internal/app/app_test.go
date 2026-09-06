package app

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/n0ah-n0wa/VitalMesh/services/api-gateway/internal/config"
	"github.com/n0ah-n0wa/VitalMesh/services/api-gateway/internal/httpapi/model"
	"github.com/n0ah-n0wa/VitalMesh/services/api-gateway/internal/requestid"
)

func testConfig(t *testing.T) config.Config {
	t.Helper()
	cfg, err := config.Load(func(string) (string, bool) { return "", false })
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	cfg.HTTP.Addr = "127.0.0.1:0"
	return cfg
}

func TestWiredHandlerServesHealthWithMiddleware(t *testing.T) {
	a := New(testConfig(t), slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil)), "v-test")

	for _, path := range []string{"/health", "/ready"} {
		rec := httptest.NewRecorder()
		a.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))

		if rec.Code != http.StatusOK {
			t.Errorf("GET %s: status = %d, want 200", path, rec.Code)
		}
		if rec.Header().Get(requestid.Header) == "" {
			t.Errorf("GET %s: no %s header, middleware chain not applied", path, requestid.Header)
		}
		var body model.Health
		if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
			t.Fatalf("GET %s: decode: %v", path, err)
		}
		if body.Service != ServiceName || body.Version != "v-test" {
			t.Errorf("GET %s: body = %+v", path, body)
		}
	}

	rec := httptest.NewRecorder()
	a.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/nope", nil))
	if rec.Code != http.StatusNotFound || rec.Header().Get("Content-Type") != "application/json" {
		t.Errorf("GET /nope: %d %q", rec.Code, rec.Header().Get("Content-Type"))
	}
}

func TestRunStopsOnContextCancel(t *testing.T) {
	a := New(testConfig(t), slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil)), "v")

	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() { errCh <- a.Run(ctx) }()

	time.Sleep(50 * time.Millisecond)
	cancel()

	select {
	case err := <-errCh:
		if err != nil {
			t.Fatalf("Run returned error: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after cancellation")
	}
}
