//go:build integration

package app

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/n0ah-n0wa/VitalMesh/services/api-gateway/internal/idempotency"
	"github.com/n0ah-n0wa/VitalMesh/services/api-gateway/internal/infra/redisclient/redistest"
	"github.com/n0ah-n0wa/VitalMesh/services/api-gateway/internal/observability/redact"
)

// What the whole gateway actually writes, driven through real requests
// against a real database (SPECIFICATIONS.md section 41). The unit tests in
// internal/observability/logging prove the rules; these prove the rules are
// in the path a request takes.

// The credentials the harness seeds. A test that asserts they never appear
// has to know them.
const (
	harnessEmail    = "redis-op@example.com"
	harnessPassword = "redis-integration-password"
)

// logLines decodes everything the gateway logged, failing if any line is
// not a JSON object: an unparseable line is a broken log pipeline.
func logLines(t *testing.T, h *redisHarness) []map[string]any {
	t.Helper()
	var records []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(h.logs.String()), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var rec map[string]any
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("log line is not JSON: %v\n%s", err, line)
		}
		records = append(records, rec)
	}
	return records
}

// requestRecords are the per-request log entries.
func requestRecords(t *testing.T, h *redisHarness) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, rec := range logLines(t, h) {
		if rec["message"] == "request" {
			out = append(out, rec)
		}
	}
	return out
}

// Every record a served request produces carries what an operator needs to
// place it: when, how bad, which service, which deployment, which request.
func TestEveryRequestRecordCarriesTheSchema(t *testing.T) {
	h := newRedisHarness(t, redistest.URL(t), nil)
	h.logs.Reset()

	rec := h.do(http.MethodGet, "/api/v1/patients/"+h.patientID.String(), "", h.token, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("read = %d: %s", rec.Code, rec.Body.String())
	}

	records := requestRecords(t, h)
	if len(records) == 0 {
		t.Fatal("the request was not logged")
	}
	got := records[0]
	for _, field := range []string{"timestamp", "level", "service", "version", "environment", "message", "request_id"} {
		if _, ok := got[field]; !ok {
			t.Errorf("the request record has no %q:\n%v", field, got)
		}
	}
	if name, _ := got["service"].(string); name == "" {
		t.Errorf("service = %v, want the process's identity", got["service"])
	}
	// Structured fields, not a sentence to be parsed later.
	for _, field := range []string{"method", "route", "status", "duration_ms"} {
		if _, ok := got[field]; !ok {
			t.Errorf("the request record has no %q:\n%v", field, got)
		}
	}
	// A request id ties every record of one request together, and the
	// client is told the same one.
	if got["request_id"] != rec.Header().Get("X-Request-ID") {
		t.Errorf("logged request_id %v, returned %q", got["request_id"], rec.Header().Get("X-Request-ID"))
	}
}

// Logging in is where a password exists at all. Neither the password nor
// the address that identifies the person may reach the log, whether the
// attempt succeeds or fails.
func TestALoginLeaksNeitherPasswordNorEmailNorToken(t *testing.T) {
	h := newRedisHarness(t, redistest.URL(t), nil)
	h.logs.Reset()

	body := `{"email":"` + harnessEmail + `","password":"` + harnessPassword + `"}`
	ok := h.do(http.MethodPost, "/api/v1/auth/login", body, "", nil)
	if ok.Code != http.StatusOK {
		t.Fatalf("login = %d: %s", ok.Code, ok.Body.String())
	}
	var issued struct {
		AccessToken string `json:"access_token"`
	}
	if err := json.Unmarshal(ok.Body.Bytes(), &issued); err != nil {
		t.Fatalf("login response: %v", err)
	}
	if issued.AccessToken == "" {
		t.Fatal("no token was issued; the test would prove nothing")
	}

	// And the failure paths, which log a reason.
	wrong := `{"email":"` + harnessEmail + `","password":"not-the-password-at-all"}`
	h.do(http.MethodPost, "/api/v1/auth/login", wrong, "", nil)
	unknown := `{"email":"nobody@example.com","password":"` + harnessPassword + `"}`
	h.do(http.MethodPost, "/api/v1/auth/login", unknown, "", nil)

	logs := h.logs.String()
	for _, secret := range []string{
		harnessPassword,
		"not-the-password-at-all",
		harnessEmail,
		"nobody@example.com",
		issued.AccessToken,
	} {
		if strings.Contains(logs, secret) {
			t.Errorf("a credential or address reached the log: %q\n%s", secret, logs)
		}
	}

	// The attempts are still recorded: redaction must not cost the audit.
	if !strings.Contains(logs, "login succeeded") || !strings.Contains(logs, "login failed") {
		t.Errorf("login attempts were not logged at all:\n%s", logs)
	}
}

// A bearer token travels on every authenticated request. It must not be
// logged, including when it is rejected.
func TestABearerTokenNeverReachesTheLog(t *testing.T) {
	h := newRedisHarness(t, redistest.URL(t), nil)
	h.logs.Reset()

	path := "/api/v1/patients/" + h.patientID.String()
	h.do(http.MethodGet, path, "", h.token, nil)

	// A syntactically valid token this gateway did not sign: the rejection
	// path formats an error, which is where a token tends to escape.
	forged := h.token[:len(h.token)-6] + "AAAAAA"
	if rec := h.do(http.MethodGet, path, "", forged, nil); rec.Code != http.StatusUnauthorized {
		t.Fatalf("a forged token = %d, want 401", rec.Code)
	}

	logs := h.logs.String()
	for _, token := range []string{h.token, forged} {
		if strings.Contains(logs, token) {
			t.Errorf("a token reached the log:\n%s", logs)
		}
	}
	// Even a fragment long enough to be useful must not survive.
	if strings.Contains(logs, h.token[:40]) {
		t.Errorf("a token prefix reached the log:\n%s", logs)
	}
	if !strings.Contains(logs, "authentication failed") {
		t.Errorf("the rejection was not logged:\n%s", logs)
	}
}

// Writing measurements is where payloads exist. The log records what
// happened and how much of it, never the readings themselves.
func TestAMeasurementWriteLogsCountsNotReadings(t *testing.T) {
	h := newRedisHarness(t, redistest.URL(t), nil)
	h.logs.Reset()

	body := `{"patient_id":"` + h.patientID.String() + `","type":"HEART_RATE",` +
		`"value":121.5,"unit":"bpm","recorded_at":"2026-09-06T11:00:00Z",` +
		`"source":"log-test-monitor","metadata":{"lead":"II"}}`
	rec := h.do(http.MethodPost, "/api/v1/measurements", body, h.token,
		map[string]string{idempotency.Header: "log-" + uuid.NewString()})
	if rec.Code != http.StatusCreated {
		t.Fatalf("write = %d: %s", rec.Code, rec.Body.String())
	}

	logs := h.logs.String()
	if strings.Contains(logs, "121.5") {
		t.Errorf("a measurement value reached the log:\n%s", logs)
	}
	if strings.Contains(logs, `"recorded_at":"2026-01-01`) {
		t.Errorf("a reading's payload reached the log:\n%s", logs)
	}
	// The event itself is recorded, with the identifiers needed to trace it.
	if !strings.Contains(logs, "measurement created") {
		t.Errorf("the write was not logged:\n%s", logs)
	}
}

// A patient's own details are what the record protects. The log names the
// patient by id and says nothing else about them.
func TestPatientDetailsAreNotLogged(t *testing.T) {
	h := newRedisHarness(t, redistest.URL(t), nil)
	h.logs.Reset()

	reference := "MRN-" + uuid.NewString()
	body := `{"external_reference":"` + reference + `","date_of_birth":"1970-03-04","sex":"FEMALE"}`
	rec := h.do(http.MethodPost, "/api/v1/patients", body, h.adminTok, nil)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create = %d: %s", rec.Code, rec.Body.String())
	}

	logs := h.logs.String()
	for _, detail := range []string{reference, "1970-03-04"} {
		if strings.Contains(logs, detail) {
			t.Errorf("a patient detail reached the log: %q\n%s", detail, logs)
		}
	}
	if !strings.Contains(logs, "patient created") {
		t.Errorf("the write was not logged:\n%s", logs)
	}
}

// The redaction rules have to hold for the running gateway's logger, not
// only for one built in a unit test.
func TestTheRunningGatewayRedactsASensitiveField(t *testing.T) {
	h := newRedisHarness(t, redistest.URL(t), nil)
	h.logs.Reset()

	h.app.logger.Info("probe", "password", "hunter2", "patient_id", h.patientID)

	logs := h.logs.String()
	if strings.Contains(logs, "hunter2") {
		t.Errorf("the gateway's own logger did not redact:\n%s", logs)
	}
	if !strings.Contains(logs, redact.Redacted) {
		t.Errorf("nothing was marked as redacted:\n%s", logs)
	}
	if !strings.Contains(logs, h.patientID.String()) {
		t.Errorf("the identifier was dropped along with the credential:\n%s", logs)
	}
}
