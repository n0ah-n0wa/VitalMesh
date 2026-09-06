package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/n0ah-n0wa/VitalMesh/services/api-gateway/internal/health"
	"github.com/n0ah-n0wa/VitalMesh/services/api-gateway/internal/httpapi/model"
)

type checker struct {
	name string
	err  error
}

func (c checker) Name() string                { return c.name }
func (c checker) Check(context.Context) error { return c.err }

func newHealth(checkers ...health.Checker) (*Health, *bytes.Buffer) {
	var logBuf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logBuf, nil))
	return NewHealth("api-gateway", "v1", health.NewReadiness(time.Second, checkers...), logger), &logBuf
}

func TestLive(t *testing.T) {
	h, _ := newHealth(checker{"postgres", errors.New("down")})
	rec := httptest.NewRecorder()

	h.Live(rec, httptest.NewRequest(http.MethodGet, "/health", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 even with failing dependencies", rec.Code)
	}
	var body model.Health
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	want := model.Health{Status: "ok", Service: "api-gateway", Version: "v1"}
	if body != want {
		t.Errorf("body = %+v, want %+v", body, want)
	}
}

func TestReadyWithoutCheckers(t *testing.T) {
	h, _ := newHealth()
	rec := httptest.NewRecorder()

	h.Ready(rec, httptest.NewRequest(http.MethodGet, "/ready", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), `"checks":[]`) {
		t.Errorf("checks should be an empty array, got %s", rec.Body.String())
	}
}

func TestReadyReportsChecksWithoutLeakingCauses(t *testing.T) {
	h, logBuf := newHealth(checker{"postgres", nil}, checker{"redis", errors.New("dial tcp 10.1.2.3:6379: refused")})
	rec := httptest.NewRecorder()

	h.Ready(rec, httptest.NewRequest(http.MethodGet, "/ready", nil))

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rec.Code)
	}
	var body model.Readiness
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.Status != "not_ready" {
		t.Errorf("status = %q, want not_ready", body.Status)
	}
	wantChecks := []model.Check{{Name: "postgres", Status: "ok"}, {Name: "redis", Status: "fail"}}
	if len(body.Checks) != len(wantChecks) {
		t.Fatalf("checks = %+v, want %+v", body.Checks, wantChecks)
	}
	for i := range wantChecks {
		if body.Checks[i] != wantChecks[i] {
			t.Errorf("checks[%d] = %+v, want %+v", i, body.Checks[i], wantChecks[i])
		}
	}
	if strings.Contains(rec.Body.String(), "10.1.2.3") {
		t.Error("readiness body leaks the failure cause")
	}
	if !strings.Contains(logBuf.String(), "10.1.2.3") {
		t.Error("failure cause was not logged server-side")
	}
}
