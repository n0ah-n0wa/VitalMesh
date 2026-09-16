package synth

import (
	"bytes"
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/n0ah-n0wa/VitalMesh/services/api-gateway/internal/synth/synthtest"
)

func writeSmall(t *testing.T, seed int64) (string, Manifest) {
	t.Helper()
	dir := t.TempDir()
	m, err := Write(small(seed), dir, false)
	if err != nil {
		t.Fatal(err)
	}
	return dir, m
}

func newLoader(t *testing.T, g *synthtest.Gateway) (*Loader, *bytes.Buffer) {
	t.Helper()
	log := &bytes.Buffer{}
	l := &Loader{Client: http.DefaultClient, BaseURL: g.URL(), BatchSize: 10, Log: log, Sleep: func(time.Duration) {}}
	if err := l.Login(context.Background(), g.Email, g.Password); err != nil {
		t.Fatalf("Login: %v", err)
	}
	return l, log
}

func TestLoadingStoresEverythingOnceAndIsIdempotent(t *testing.T) {
	g := synthtest.New(t, EnvLocal, "op@x.invalid", "pw-pw-pw-pw-pw")
	g.RateLimitOnce = true
	dir, m := writeSmall(t, 42)
	l, log := newLoader(t, g)
	l.Jobs = true

	r, err := l.Load(context.Background(), dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if r.Patients.Created != 2 || r.Patients.Existing != 0 {
		t.Errorf("patients %+v", r.Patients)
	}
	if r.Measurements.Stored != m.Counts.Measurements || r.Measurements.Existing != 0 {
		t.Errorf("measurements %+v, fixture has %d", r.Measurements, m.Counts.Measurements)
	}
	if r.Jobs.Completed != 2 || r.Jobs.Failed != 0 || g.Jobs() != 2 {
		t.Errorf("jobs %+v (gateway saw %d)", r.Jobs, g.Jobs())
	}
	if g.MeasurementCount() != m.Counts.Measurements {
		t.Errorf("gateway holds %d readings, want %d", g.MeasurementCount(), m.Counts.Measurements)
	}
	if !strings.Contains(log.String(), "RATE_LIMIT_EXCEEDED") || !strings.Contains(log.String(), "retrying in") {
		t.Errorf("the 429 should be logged and retried:\n%s", log)
	}

	// Every write carries a key derived from the fixture, and a batch
	// never spans two patients.
	keys := map[string]int{}
	for _, req := range g.Requests() {
		if req.Method == http.MethodPost && req.Path != "/api/v1/auth/login" {
			if req.IdempotencyKey == "" {
				t.Errorf("%s %s without an Idempotency-Key", req.Method, req.Path)
			}
			keys[req.IdempotencyKey]++
		}
	}
	patients := g.Patients()
	for key := range keys {
		if strings.HasPrefix(key, "synth:batch:") && !strings.Contains(key, patients[0].ExternalReference) && !strings.Contains(key, patients[1].ExternalReference) {
			t.Errorf("batch key %q names no fixture patient", key)
		}
	}

	// A second load of the same fixture changes nothing.
	again, err := l.Load(context.Background(), dir)
	if err != nil {
		t.Fatalf("second Load: %v", err)
	}
	if again.Patients.Created != 0 || again.Patients.Existing != 2 || again.Measurements.Stored != 0 || again.Measurements.Existing != m.Counts.Measurements {
		t.Errorf("second load: %+v", again)
	}
	if g.MeasurementCount() != m.Counts.Measurements || len(g.Patients()) != 2 {
		t.Errorf("second load changed the gateway: %d readings, %d patients", g.MeasurementCount(), len(g.Patients()))
	}
}

func TestLoadingWhenTheIdempotencyRecordsAreGoneFallsBackToConflicts(t *testing.T) {
	g := synthtest.New(t, EnvLocal, "op@x.invalid", "pw-pw-pw-pw-pw")
	dir, m := writeSmall(t, 3)
	l, _ := newLoader(t, g)
	if _, err := l.Load(context.Background(), dir); err != nil {
		t.Fatal(err)
	}
	g.ForgetIdempotency = true

	r, err := l.Load(context.Background(), dir)
	if err != nil {
		t.Fatalf("Load after the records expired: %v", err)
	}
	if r.Patients.Existing != 2 || r.Patients.Created != 0 {
		t.Errorf("patients should be found through the list: %+v", r.Patients)
	}
	if r.Measurements.Existing != m.Counts.Measurements || r.Measurements.Stored != 0 {
		t.Errorf("readings should be recognised one by one: %+v", r.Measurements)
	}
	if g.MeasurementCount() != m.Counts.Measurements {
		t.Errorf("gateway holds %d readings, want %d unchanged", g.MeasurementCount(), m.Counts.Measurements)
	}
	listed := false
	for _, req := range g.Requests() {
		if req.Method == http.MethodGet && req.Path == "/api/v1/patients" {
			listed = true
		}
	}
	if !listed {
		t.Error("the loader should have walked the patient list to find the existing patients")
	}
}

func TestLoadingFailsClearly(t *testing.T) {
	g := synthtest.New(t, EnvLocal, "op@x.invalid", "pw-pw-pw-pw-pw")
	l := &Loader{Client: http.DefaultClient, BaseURL: g.URL(), Sleep: func(time.Duration) {}}
	if err := l.Login(context.Background(), "op@x.invalid", "wrong-wrong-wrong"); err == nil || !strings.Contains(err.Error(), "INVALID_CREDENTIALS") {
		t.Errorf("a bad sign-in must name the API error, got %v", err)
	}
	if _, err := l.Load(context.Background(), t.TempDir()); err == nil || !strings.Contains(err.Error(), "not signed in") {
		t.Errorf("Load before Login = %v", err)
	}
	if err := l.Login(context.Background(), g.Email, g.Password); err != nil {
		t.Fatal(err)
	}
	if _, err := l.Load(context.Background(), t.TempDir()); err == nil || !strings.Contains(err.Error(), "not a fixture directory") {
		t.Errorf("Load of an empty dir = %v", err)
	}

	// A gateway that stays unavailable is given up on after the retries.
	down := &Loader{Client: &http.Client{Timeout: time.Second}, BaseURL: "http://127.0.0.1:1", MaxRetries: 2, Sleep: func(time.Duration) {}}
	err := down.Login(context.Background(), "a@b.invalid", "pw-pw-pw-pw-pw")
	if err == nil || !strings.Contains(err.Error(), "after 3 attempts") {
		t.Errorf("Login against nothing = %v, want a bounded retry", err)
	}
}

func TestErrorEnvelopesAreRenderedWithoutRawBodies(t *testing.T) {
	got := apiError(422, []byte(`{"error":{"code":"MEASUREMENT_VALIDATION_FAILED","message":"validation failed","details":[{"field":"items[0].value","message":"must be within 0 and 300"}]}}`))
	if got != "HTTP 422 MEASUREMENT_VALIDATION_FAILED: validation failed; items[0].value must be within 0 and 300" {
		t.Errorf("apiError = %q", got)
	}
	if got := apiError(502, []byte(`<html>proxy error</html>`)); got != "HTTP 502" {
		t.Errorf("a non-JSON body must not be echoed, got %q", got)
	}
	if apiCode([]byte(`{"error":{"code":"X"}}`)) != "X" || apiCode(nil) != "" {
		t.Error("apiCode")
	}
	h := http.Header{"Retry-After": []string{"3"}}
	if retryAfter(h, time.Second) != 3*time.Second || retryAfter(http.Header{}, time.Second) != time.Second {
		t.Error("retryAfter")
	}
	h.Set("Retry-After", "3600")
	if retryAfter(h, time.Second) != time.Minute {
		t.Error("retryAfter must cap a long wait")
	}
}
