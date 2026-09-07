package app

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"runtime"
	"testing"
	"time"

	"github.com/n0ah-n0wa/VitalMesh/services/api-gateway/internal/config"
	"github.com/n0ah-n0wa/VitalMesh/services/api-gateway/internal/httpapi/model"
	"github.com/n0ah-n0wa/VitalMesh/services/api-gateway/internal/requestid"
)

// unreachableDatabase is a syntactically valid URL nothing listens on. The
// pool connects lazily, so wiring succeeds and readiness reports the outage.
const unreachableDatabase = "postgres://vitalmesh:vitalmesh@127.0.0.1:1/vitalmesh?sslmode=disable"

func testConfig(t *testing.T) config.Config {
	t.Helper()
	cfg, err := config.Load(func(key string) (string, bool) {
		switch key {
		case "DATABASE_URL":
			return unreachableDatabase, true
		case "DATABASE_CONNECT_TIMEOUT", "READINESS_TIMEOUT":
			return "200ms", true
		case "HTTP_SHUTDOWN_TIMEOUT":
			// Short, so a shutdown that waits on something reports an error
			// instead of stalling the test.
			return "1s", true
		case "JWT_SECRET":
			return "test-secret-test-secret-test-secret-32", true
		case "PASSWORD_HASH_MEMORY_KIB":
			return "8192", true
		case "PASSWORD_HASH_TIME":
			return "1", true
		}
		return "", false
	})
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	cfg.HTTP.Addr = "127.0.0.1:0"
	return cfg
}

func newApp(t *testing.T) *App {
	t.Helper()
	a, err := New(context.Background(), testConfig(t), slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil)), "v-test")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return a
}

func TestWiredHandlerServesHealthWithMiddleware(t *testing.T) {
	a := newApp(t)
	defer a.pool.Close()

	rec := httptest.NewRecorder()
	a.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/health", nil))
	if rec.Code != http.StatusOK {
		t.Errorf("GET /health: status = %d, want 200", rec.Code)
	}
	if rec.Header().Get(requestid.Header) == "" {
		t.Errorf("GET /health: no %s header, middleware chain not applied", requestid.Header)
	}
	var body model.Health
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.Service != ServiceName || body.Version != "v-test" {
		t.Errorf("body = %+v", body)
	}

	rec = httptest.NewRecorder()
	a.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/nope", nil))
	if rec.Code != http.StatusNotFound || rec.Header().Get("Content-Type") != "application/json" {
		t.Errorf("GET /nope: %d %q", rec.Code, rec.Header().Get("Content-Type"))
	}
}

func TestReadinessReportsTheDatabaseCheck(t *testing.T) {
	a := newApp(t)
	defer a.pool.Close()

	rec := httptest.NewRecorder()
	a.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/ready", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("GET /ready with unreachable database: status = %d, want 503", rec.Code)
	}
	var body model.Readiness
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.Status != model.StatusNotReady || len(body.Checks) != 1 || body.Checks[0].Name != "postgres" || body.Checks[0].Status != model.StatusFail {
		t.Errorf("body = %+v", body)
	}
}

func TestRunStopsOnContextCancel(t *testing.T) {
	a := newApp(t)

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
		// Dump every goroutine so a stall under CI load can be diagnosed.
		buf := make([]byte, 1<<20)
		t.Fatalf("Run did not return after cancellation; goroutines:\n%s", buf[:runtime.Stack(buf, true)])
	}
}
