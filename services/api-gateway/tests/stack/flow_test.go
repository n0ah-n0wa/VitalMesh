//go:build stack

package stack

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

// ------------------------------------------------------- authentication

func (s *stack) testAuthentication(t *testing.T) {
	r := s.do(t, http.MethodPost, "/api/v1/auth/login", "", map[string]string{"email": strings.ToUpper(s.operator.email), "password": password}, nil)
	r.expect(t, http.StatusOK, "")
	var login struct {
		AccessToken string    `json:"access_token"`
		TokenType   string    `json:"token_type"`
		ExpiresIn   int       `json:"expires_in"`
		ExpiresAt   time.Time `json:"expires_at"`
		User        struct {
			ID     uuid.UUID `json:"id"`
			Email  string    `json:"email"`
			Role   string    `json:"role"`
			Status string    `json:"status"`
		} `json:"user"`
	}
	r.decode(t, &login)
	if login.TokenType != "Bearer" || login.AccessToken == "" || login.ExpiresIn <= 0 || login.ExpiresAt.Before(time.Now()) {
		t.Errorf("login response: %+v", login)
	}
	if login.User.Email != s.operator.email || login.User.Role != "OPERATOR" || login.User.Status != "ACTIVE" || login.User.ID != s.operator.id {
		t.Errorf("login user: %+v (email matching is case-insensitive)", login.User)
	}
	if strings.Contains(string(r.body), password) {
		t.Error("the password came back in the login response")
	}

	me := s.do(t, http.MethodGet, "/api/v1/auth/me", login.AccessToken, nil, nil).expect(t, http.StatusOK, "")
	var who struct {
		ID   uuid.UUID `json:"id"`
		Role string    `json:"role"`
	}
	me.decode(t, &who)
	if who.ID != s.operator.id || who.Role != "OPERATOR" {
		t.Errorf("/auth/me = %+v", who)
	}

	// Wrong password and unknown account are the same answer, so the
	// response never says whether the email exists.
	wrong := s.do(t, http.MethodPost, "/api/v1/auth/login", "", map[string]string{"email": s.operator.email, "password": "not-the-password"}, nil)
	unknown := s.do(t, http.MethodPost, "/api/v1/auth/login", "", map[string]string{"email": "nobody-" + s.runID + "@vitalmesh.invalid", "password": password}, nil)
	wrong.expect(t, http.StatusUnauthorized, "INVALID_CREDENTIALS")
	unknown.expect(t, http.StatusUnauthorized, "INVALID_CREDENTIALS")
	if wrong.envelope().Error.Message != unknown.envelope().Error.Message {
		t.Errorf("a wrong password and an unknown account must read the same: %q vs %q", wrong.envelope().Error.Message, unknown.envelope().Error.Message)
	}
	for _, r := range []response{wrong, unknown} {
		if !strings.HasPrefix(r.header.Get("WWW-Authenticate"), "Bearer") {
			t.Errorf("401 without a WWW-Authenticate challenge: %v", r.header)
		}
	}

	none := s.do(t, http.MethodGet, "/api/v1/auth/me", "", nil, nil).expect(t, http.StatusUnauthorized, "AUTHENTICATION_REQUIRED")
	if !strings.HasPrefix(none.header.Get("WWW-Authenticate"), "Bearer") {
		t.Error("401 without a WWW-Authenticate challenge")
	}
	s.do(t, http.MethodGet, "/api/v1/auth/me", "not.a.token", nil, nil).expect(t, http.StatusUnauthorized, "INVALID_TOKEN")

	// A forged token: the real one with its signature changed.
	parts := strings.Split(login.AccessToken, ".")
	if len(parts) != 3 {
		t.Fatalf("token is not a JWT: %d parts", len(parts))
	}
	forged := parts[0] + "." + parts[1] + "." + strings.Repeat("A", len(parts[2]))
	s.do(t, http.MethodGet, "/api/v1/auth/me", forged, nil, nil).expect(t, http.StatusUnauthorized, "INVALID_TOKEN")

	for _, h := range []string{"X-Content-Type-Options", "Cache-Control", "X-Request-ID"} {
		if r.header.Get(h) == "" {
			t.Errorf("response lacks the %s header", h)
		}
	}
}

// ----------------------------------------------------- patient creation

func (s *stack) testPatientCreation(t *testing.T) {
	ref := "e2e-" + s.runID + "-patient"
	body := map[string]string{"external_reference": ref, "date_of_birth": "1984-02-29", "sex": "FEMALE"}
	r := s.do(t, http.MethodPost, "/api/v1/patients", s.operator.token, body, nil).expect(t, http.StatusCreated, "")
	var created struct {
		ID                uuid.UUID `json:"id"`
		ExternalReference string    `json:"external_reference"`
		DateOfBirth       string    `json:"date_of_birth"`
		Sex               string    `json:"sex"`
		Status            string    `json:"status"`
		CreatedAt         time.Time `json:"created_at"`
	}
	r.decode(t, &created)
	if created.ID == uuid.Nil || created.ExternalReference != ref || created.DateOfBirth != "1984-02-29" || created.Sex != "FEMALE" || created.Status != "ACTIVE" {
		t.Errorf("created patient: %+v", created)
	}
	if loc := r.header.Get("Location"); !strings.HasSuffix(loc, "/api/v1/patients/"+created.ID.String()) {
		t.Errorf("Location = %q", loc)
	}

	got := s.do(t, http.MethodGet, "/api/v1/patients/"+created.ID.String(), s.user.token, nil, nil).expect(t, http.StatusOK, "")
	var read struct {
		ID uuid.UUID `json:"id"`
	}
	got.decode(t, &read)
	if read.ID != created.ID {
		t.Errorf("GET returned %s, want %s", read.ID, created.ID)
	}

	// The list holds it, in creation order, and pages.
	list := s.do(t, http.MethodGet, "/api/v1/patients?limit=1", s.operator.token, nil, nil).expect(t, http.StatusOK, "")
	var page struct {
		Items      []json.RawMessage `json:"items"`
		NextCursor *string           `json:"next_cursor"`
		HasMore    bool              `json:"has_more"`
	}
	list.decode(t, &page)
	if len(page.Items) != 1 {
		t.Errorf("limit=1 returned %d items", len(page.Items))
	}

	// Persisted as sent.
	rows := s.query(t, "SELECT external_reference, sex, status FROM patients WHERE id = $1", created.ID)
	if len(rows) != 1 || rows[0]["external_reference"] != ref || rows[0]["sex"] != "FEMALE" || rows[0]["status"] != "ACTIVE" {
		t.Errorf("patient row: %v", rows)
	}

	// The same reference again is a conflict, not a second patient.
	s.do(t, http.MethodPost, "/api/v1/patients", s.operator.token, body, nil).expect(t, http.StatusConflict, "PATIENT_ALREADY_EXISTS")
	if n := s.count(t, "SELECT count(*) FROM patients WHERE external_reference = $1", ref); n != 1 {
		t.Errorf("%d patients with reference %s", n, ref)
	}

	// Unknown and malformed ids are the same 404, so an id cannot be probed.
	s.do(t, http.MethodGet, "/api/v1/patients/"+uuid.New().String(), s.operator.token, nil, nil).expect(t, http.StatusNotFound, "PATIENT_NOT_FOUND")
	s.do(t, http.MethodGet, "/api/v1/patients/not-a-uuid", s.operator.token, nil, nil).expect(t, http.StatusNotFound, "PATIENT_NOT_FOUND")

	// Soft delete: gone for an operator, visible as DELETED to an admin.
	victim := s.patient(t, "deleted")
	s.do(t, http.MethodDelete, "/api/v1/patients/"+victim.String(), s.operator.token, nil, nil).expect(t, http.StatusNoContent, "")
	s.do(t, http.MethodGet, "/api/v1/patients/"+victim.String(), s.operator.token, nil, nil).expect(t, http.StatusNotFound, "PATIENT_NOT_FOUND")
	asAdmin := s.do(t, http.MethodGet, "/api/v1/patients/"+victim.String(), s.admin.token, nil, nil).expect(t, http.StatusOK, "")
	var deleted struct {
		Status string `json:"status"`
	}
	asAdmin.decode(t, &deleted)
	if deleted.Status != "DELETED" {
		t.Errorf("admin sees status %q, want DELETED", deleted.Status)
	}
	s.do(t, http.MethodDelete, "/api/v1/patients/"+victim.String(), s.operator.token, nil, nil).expect(t, http.StatusConflict, "PATIENT_ALREADY_DELETED")
}

// ------------------------------------------------ measurement ingestion

func (s *stack) testMeasurementIngestion(t *testing.T) {
	patient := s.patient(t, "single")
	one := s.reading(patient, "SPO2", 97, "%", 0, "e2e-oximeter")
	one["metadata"] = map[string]any{"lead": "II"}
	r := s.do(t, http.MethodPost, "/api/v1/measurements", s.operator.token, one, nil).expect(t, http.StatusCreated, "")
	var created struct {
		ID         uuid.UUID       `json:"id"`
		PatientID  uuid.UUID       `json:"patient_id"`
		Type       string          `json:"type"`
		Value      float64         `json:"value"`
		Unit       string          `json:"unit"`
		RecordedAt time.Time       `json:"recorded_at"`
		Source     string          `json:"source"`
		Metadata   json.RawMessage `json:"metadata"`
	}
	r.decode(t, &created)
	if created.PatientID != patient || created.Type != "SPO2" || created.Value != 97 || created.Unit != "%" || created.Source != "e2e-oximeter" || !created.RecordedAt.Equal(s.base) {
		t.Errorf("created reading: %+v", created)
	}
	if !strings.Contains(string(created.Metadata), `"lead"`) {
		t.Errorf("metadata not kept: %s", created.Metadata)
	}
	if loc := r.header.Get("Location"); !strings.HasSuffix(loc, "/api/v1/measurements/"+created.ID.String()) {
		t.Errorf("Location = %q", loc)
	}

	s.do(t, http.MethodGet, "/api/v1/measurements/"+created.ID.String(), s.user.token, nil, nil).expect(t, http.StatusOK, "")
	rows := s.query(t, "SELECT value, unit, source FROM measurements WHERE id = $1", created.ID)
	if len(rows) != 1 || rows[0]["unit"] != "%" || rows[0]["source"] != "e2e-oximeter" {
		t.Errorf("measurement row: %v", rows)
	}

	// The same reading (patient, type, time, source) is refused, and a
	// different source at the same time is another reading.
	s.do(t, http.MethodPost, "/api/v1/measurements", s.operator.token, one, nil).expect(t, http.StatusConflict, "MEASUREMENT_ALREADY_EXISTS")
	other := s.reading(patient, "SPO2", 96, "%", 0, "e2e-other-oximeter")
	s.do(t, http.MethodPost, "/api/v1/measurements", s.operator.token, other, nil).expect(t, http.StatusCreated, "")

	// Listing, filtered by type and time.
	list := s.do(t, http.MethodGet, fmt.Sprintf("/api/v1/patients/%s/measurements?type=SPO2&from=%s&to=%s", patient,
		s.base.Format(time.RFC3339), s.base.Add(time.Minute).Format(time.RFC3339)), s.user.token, nil, nil).expect(t, http.StatusOK, "")
	var page struct {
		Items []struct {
			Source string `json:"source"`
		} `json:"items"`
		HasMore bool `json:"has_more"`
	}
	list.decode(t, &page)
	if len(page.Items) != 2 || page.HasMore {
		t.Errorf("listing = %+v, want the two readings", page)
	}
	s.do(t, http.MethodGet, fmt.Sprintf("/api/v1/patients/%s/measurements?type=HEART_RATE", patient), s.user.token, nil, nil).expect(t, http.StatusOK, "")

	// Deleting is a hard delete that the audit log records.
	s.do(t, http.MethodDelete, "/api/v1/measurements/"+created.ID.String(), s.operator.token, nil, nil).expect(t, http.StatusNoContent, "")
	s.do(t, http.MethodGet, "/api/v1/measurements/"+created.ID.String(), s.operator.token, nil, nil).expect(t, http.StatusNotFound, "MEASUREMENT_NOT_FOUND")
	if n := s.count(t, "SELECT count(*) FROM audit_logs WHERE action = 'MEASUREMENT_DELETED' AND resource_id = $1", created.ID); n != 1 {
		t.Errorf("%d MEASUREMENT_DELETED audit rows, want 1", n)
	}
}

// ---------------------------------------------------- batch ingestion

func (s *stack) testBatchIngestion(t *testing.T) {
	patient := s.patient(t, "batch")
	items := s.heartRateSeries(patient, "e2e-monitor")
	r := s.do(t, http.MethodPost, "/api/v1/measurements/batch", s.operator.token, map[string]any{"items": items}, nil).expect(t, http.StatusCreated, "")
	var out struct {
		Items []struct {
			ID    uuid.UUID `json:"id"`
			Value float64   `json:"value"`
		} `json:"items"`
	}
	r.decode(t, &out)
	if len(out.Items) != 60 || out.Items[30].Value != 185 {
		t.Fatalf("batch stored %d items (item 30 = %v), want 60 in input order", len(out.Items), out.Items[min(30, len(out.Items)-1)].Value)
	}
	if n := s.count(t, "SELECT count(*) FROM measurements WHERE patient_id = $1", patient); n != 60 {
		t.Errorf("%d rows stored, want 60", n)
	}

	// All or nothing: one bad item and none of a batch is stored.
	bad := s.heartRateSeries(patient, "e2e-second-monitor")
	bad[7]["value"] = 999 // above the technical range
	rr := s.do(t, http.MethodPost, "/api/v1/measurements/batch", s.operator.token, map[string]any{"items": bad}, nil).expect(t, http.StatusUnprocessableEntity, "MEASUREMENT_VALIDATION_FAILED")
	if rr.detail("items[7].value") == "" {
		t.Errorf("the failing item is not named: %s", rr.body)
	}
	if n := s.count(t, "SELECT count(*) FROM measurements WHERE patient_id = $1 AND source = 'e2e-second-monitor'", patient); n != 0 {
		t.Errorf("%d rows of a rejected batch were stored", n)
	}

	// A batch that repeats a stored reading is refused whole, naming the item.
	again := s.heartRateSeries(patient, "e2e-monitor")[:3]
	dup := s.do(t, http.MethodPost, "/api/v1/measurements/batch", s.operator.token, map[string]any{"items": again}, nil).expect(t, http.StatusConflict, "MEASUREMENT_ALREADY_EXISTS")
	if !strings.Contains(string(dup.body), "items[") {
		t.Errorf("the duplicate item is not named: %s", dup.body)
	}
	// Two items describing the same reading, inside one batch.
	twin := []map[string]any{s.reading(patient, "SPO2", 98, "%", 0, "e2e-oximeter"), s.reading(patient, "SPO2", 97, "%", 0, "e2e-oximeter")}
	within := s.do(t, http.MethodPost, "/api/v1/measurements/batch", s.operator.token, map[string]any{"items": twin}, nil).expect(t, http.StatusUnprocessableEntity, "MEASUREMENT_VALIDATION_FAILED")
	if !strings.Contains(within.detail("items[1]"), "duplicates") {
		t.Errorf("the in-batch duplicate is not reported: %s", within.body)
	}
	// Empty and oversized batches.
	s.do(t, http.MethodPost, "/api/v1/measurements/batch", s.operator.token, map[string]any{"items": []any{}}, nil).expect(t, http.StatusUnprocessableEntity, "MEASUREMENT_VALIDATION_FAILED")
	if n := s.count(t, "SELECT count(*) FROM measurements WHERE patient_id = $1", patient); n != 60 {
		t.Errorf("%d rows after the refused batches, want still 60", n)
	}
}

// ------------------------------------- processing: Go to Rust and back

// jobView is the job the API returns.
type jobView struct {
	ID               uuid.UUID `json:"id"`
	PatientID        uuid.UUID `json:"patient_id"`
	Status           string    `json:"status"`
	AlgorithmVersion string    `json:"algorithm_version"`
	ServiceVersion   *string   `json:"service_version"`
	ErrorCode        *string   `json:"error_code"`
	AttemptCount     int       `json:"attempt_count"`
	CompletedAt      *string   `json:"completed_at"`
	FailedAt         *string   `json:"failed_at"`
}

func (s *stack) testProcessing(t *testing.T) {
	patient := s.patient(t, "processing")
	s.do(t, http.MethodPost, "/api/v1/measurements/batch", s.operator.token, map[string]any{"items": s.heartRateSeries(patient, "e2e-monitor")}, nil).expect(t, http.StatusCreated, "")

	correlation := "e2e-corr-" + s.runID
	r := s.do(t, http.MethodPost, "/api/v1/processing/jobs", s.operator.token, s.jobRequest(patient), map[string]string{"X-Correlation-ID": correlation}).expect(t, http.StatusCreated, "")
	var job jobView
	r.decode(t, &job)
	if job.Status != "COMPLETED" || job.PatientID != patient || job.CompletedAt == nil || job.ErrorCode != nil {
		t.Fatalf("job = %+v, want COMPLETED", job)
	}
	if job.ServiceVersion == nil || *job.ServiceVersion == "" || job.AlgorithmVersion == "" {
		t.Errorf("a completed job must carry the processor's service and algorithm versions: %+v", job)
	}
	if loc := r.header.Get("Location"); !strings.HasSuffix(loc, "/api/v1/processing/jobs/"+job.ID.String()) {
		t.Errorf("Location = %q", loc)
	}

	// Read back, and the row behind it: what the Rust service reported,
	// and the identifiers of the request that asked, are on record.
	back := s.do(t, http.MethodGet, "/api/v1/processing/jobs/"+job.ID.String(), s.user.token, nil, nil).expect(t, http.StatusOK, "")
	var read jobView
	back.decode(t, &read)
	if read.ID != job.ID || read.Status != "COMPLETED" {
		t.Errorf("job read back = %+v", read)
	}
	rows := s.query(t, "SELECT status, service_version, request_id, trace_id, attempt_count FROM processing_jobs WHERE id = $1", job.ID)
	if len(rows) != 1 {
		t.Fatalf("%d job rows", len(rows))
	}
	row := rows[0]
	if row["status"] != "COMPLETED" || row["service_version"] == nil || row["request_id"] != r.requestID() {
		t.Errorf("job row = %v; request id of the call was %s", row, r.requestID())
	}
	if row["trace_id"] == nil || row["trace_id"] == "" {
		t.Errorf("the job row carries no trace id: %v", row)
	}

	// Results: stored in the same transaction as the transition and
	// returned through the API; the planted 185 bpm is found.
	results := s.do(t, http.MethodGet, "/api/v1/patients/"+patient.String()+"/processing-results?limit=200", s.user.token, nil, nil).expect(t, http.StatusOK, "")
	var page struct {
		Items []struct {
			JobID           uuid.UUID       `json:"job_id"`
			MeasurementType string          `json:"measurement_type"`
			Window          string          `json:"window"`
			Statistics      json.RawMessage `json:"statistics"`
			Anomalies       json.RawMessage `json:"anomalies"`
			ServiceVersion  string          `json:"service_version"`
		} `json:"items"`
	}
	results.decode(t, &page)
	if len(page.Items) == 0 {
		t.Fatal("no results returned for a completed job")
	}
	windows := map[string]int{}
	anomalies := 0
	for _, it := range page.Items {
		if it.JobID != job.ID || it.MeasurementType != "HEART_RATE" {
			t.Errorf("result belongs to job %s type %s", it.JobID, it.MeasurementType)
		}
		windows[it.Window]++
		var stats map[string]any
		if err := json.Unmarshal(it.Statistics, &stats); err != nil || len(stats) == 0 {
			t.Errorf("result statistics are empty: %s", it.Statistics)
		}
		var found []map[string]any
		_ = json.Unmarshal(it.Anomalies, &found)
		anomalies += len(found)
	}
	if windows["5m"] == 0 || windows["1h"] == 0 {
		t.Errorf("results cover windows %v, want both 5m and 1h", windows)
	}
	if anomalies == 0 {
		t.Error("no anomaly was found though one reading was 185 bpm: the processor's rules did not apply")
	}
	if n := s.count(t, "SELECT count(*) FROM processing_results WHERE job_id = $1", job.ID); n != int64(len(page.Items)) {
		t.Errorf("%d result rows persisted, API returned %d", n, len(page.Items))
	}
	if n := s.count(t, "SELECT count(*) FROM processing_results WHERE job_id = $1 AND service_version = $2", job.ID, *job.ServiceVersion); n != int64(len(page.Items)) {
		t.Errorf("results do not carry the processor version %q", *job.ServiceVersion)
	}

	// A range with nothing in it is a refused, recorded FAILED job.
	empty := s.jobRequest(patient)
	empty["from"], empty["to"] = "2000-01-01T00:00:00Z", "2000-01-02T00:00:00Z"
	rr := s.do(t, http.MethodPost, "/api/v1/processing/jobs", s.operator.token, empty, nil).expect(t, http.StatusUnprocessableEntity, "PROCESSING_NO_MEASUREMENTS")
	if rr.envelope().Error.RequestID == "" {
		t.Error("error envelope without request_id")
	}
	if n := s.count(t, "SELECT count(*) FROM processing_jobs WHERE patient_id = $1 AND status = 'FAILED'", patient); n != 1 {
		t.Errorf("%d FAILED jobs recorded for the empty range, want 1", n)
	}
	if n := s.count(t, "SELECT count(*) FROM processing_jobs WHERE patient_id = $1 AND status IN ('PENDING','PROCESSING')", patient); n != 0 {
		t.Errorf("%d jobs left non-terminal", n)
	}
	s.do(t, http.MethodGet, "/api/v1/processing/jobs/"+uuid.New().String(), s.user.token, nil, nil).expect(t, http.StatusNotFound, "PROCESSING_JOB_NOT_FOUND")
}

// -------------------------------------------------------- authorization

func (s *stack) testAuthorization(t *testing.T) {
	patient := s.patient(t, "authz")
	reading := s.reading(patient, "SPO2", 95, "%", 5, "e2e-authz")

	// USER: reads yes, writes no; and the refusal happens before validation.
	s.do(t, http.MethodGet, "/api/v1/patients/"+patient.String(), s.user.token, nil, nil).expect(t, http.StatusOK, "")
	s.do(t, http.MethodGet, "/api/v1/patients/"+patient.String()+"/measurements", s.user.token, nil, nil).expect(t, http.StatusOK, "")
	s.do(t, http.MethodPost, "/api/v1/patients", s.user.token, map[string]string{"external_reference": "e2e-" + s.runID + "-forbidden", "date_of_birth": "1990-01-01", "sex": "MALE"}, nil).expect(t, http.StatusForbidden, "PERMISSION_DENIED")
	s.do(t, http.MethodPost, "/api/v1/measurements", s.user.token, reading, nil).expect(t, http.StatusForbidden, "PERMISSION_DENIED")
	s.do(t, http.MethodPost, "/api/v1/measurements/batch", s.user.token, map[string]any{"items": []any{reading}}, nil).expect(t, http.StatusForbidden, "PERMISSION_DENIED")
	s.do(t, http.MethodPost, "/api/v1/processing/jobs", s.user.token, s.jobRequest(patient), nil).expect(t, http.StatusForbidden, "PERMISSION_DENIED")
	s.do(t, http.MethodDelete, "/api/v1/patients/"+patient.String(), s.user.token, nil, nil).expect(t, http.StatusForbidden, "PERMISSION_DENIED")
	s.do(t, http.MethodPost, "/api/v1/patients", s.user.token, "{not json", nil).expect(t, http.StatusForbidden, "PERMISSION_DENIED")
	if n := s.count(t, "SELECT count(*) FROM patients WHERE external_reference = $1", "e2e-"+s.runID+"-forbidden"); n != 0 {
		t.Error("a forbidden request created a patient")
	}

	// OPERATOR and ADMIN write; the operator's write is visible to the user.
	created := s.do(t, http.MethodPost, "/api/v1/measurements", s.operator.token, reading, nil).expect(t, http.StatusCreated, "")
	var m struct {
		ID uuid.UUID `json:"id"`
	}
	created.decode(t, &m)
	s.do(t, http.MethodGet, "/api/v1/measurements/"+m.ID.String(), s.user.token, nil, nil).expect(t, http.StatusOK, "")
	s.do(t, http.MethodDelete, "/api/v1/measurements/"+m.ID.String(), s.admin.token, nil, nil).expect(t, http.StatusNoContent, "")

	// Nothing but the token decides: a role claimed in a header is ignored.
	s.do(t, http.MethodPost, "/api/v1/measurements", s.user.token, reading, map[string]string{"X-Role": "ADMIN"}).expect(t, http.StatusForbidden, "PERMISSION_DENIED")
	// And no token at all is 401, not 403, whatever the operation.
	s.do(t, http.MethodPost, "/api/v1/patients", "", map[string]string{}, nil).expect(t, http.StatusUnauthorized, "AUTHENTICATION_REQUIRED")
}

// ---------------------------------------------------------- idempotency

func (s *stack) testIdempotency(t *testing.T) {
	key := "e2e-" + s.runID + "-patient-key"
	body := map[string]string{"external_reference": "e2e-" + s.runID + "-idem", "date_of_birth": "1975-06-15", "sex": "MALE"}
	first := s.do(t, http.MethodPost, "/api/v1/patients", s.operator.token, body, map[string]string{"Idempotency-Key": key}).expect(t, http.StatusCreated, "")
	if first.header.Get("Idempotency-Replayed") != "" {
		t.Error("the first request was marked as a replay")
	}
	replay := s.do(t, http.MethodPost, "/api/v1/patients", s.operator.token, body, map[string]string{"Idempotency-Key": key}).expect(t, http.StatusCreated, "")
	if replay.header.Get("Idempotency-Replayed") != "true" {
		t.Errorf("replay not marked: %v", replay.header)
	}
	var a, b struct {
		ID uuid.UUID `json:"id"`
	}
	first.decode(t, &a)
	replay.decode(t, &b)
	if a.ID != b.ID {
		t.Errorf("replay returned patient %s, original %s", b.ID, a.ID)
	}
	if replay.header.Get("Location") != first.header.Get("Location") {
		t.Errorf("Location not replayed: %q vs %q", replay.header.Get("Location"), first.header.Get("Location"))
	}
	if replay.requestID() == first.requestID() || replay.requestID() == "" {
		t.Errorf("a replay carries its own X-Request-ID: %q vs %q", replay.requestID(), first.requestID())
	}
	if n := s.count(t, "SELECT count(*) FROM patients WHERE external_reference = $1", body["external_reference"]); n != 1 {
		t.Errorf("%d patients created under one key", n)
	}

	// Same key, different body: refused, and the original stays.
	changed := map[string]string{"external_reference": "e2e-" + s.runID + "-idem-changed", "date_of_birth": "1975-06-15", "sex": "MALE"}
	s.do(t, http.MethodPost, "/api/v1/patients", s.operator.token, changed, map[string]string{"Idempotency-Key": key}).expect(t, http.StatusUnprocessableEntity, "IDEMPOTENCY_KEY_REUSED")
	if n := s.count(t, "SELECT count(*) FROM patients WHERE external_reference = $1", changed["external_reference"]); n != 0 {
		t.Error("a refused reuse created a patient")
	}

	// Keys are scoped to the account: the admin's use of the same key is
	// its own request.
	adminBody := map[string]string{"external_reference": "e2e-" + s.runID + "-idem-admin", "date_of_birth": "1975-06-15", "sex": "MALE"}
	s.do(t, http.MethodPost, "/api/v1/patients", s.admin.token, adminBody, map[string]string{"Idempotency-Key": key}).expect(t, http.StatusCreated, "")

	// A 4xx is stored and replayed like a success.
	conflictKey := key + "-conflict"
	s.do(t, http.MethodPost, "/api/v1/patients", s.operator.token, body, map[string]string{"Idempotency-Key": conflictKey}).expect(t, http.StatusConflict, "PATIENT_ALREADY_EXISTS")
	rep := s.do(t, http.MethodPost, "/api/v1/patients", s.operator.token, body, map[string]string{"Idempotency-Key": conflictKey}).expect(t, http.StatusConflict, "PATIENT_ALREADY_EXISTS")
	if rep.header.Get("Idempotency-Replayed") != "true" {
		t.Error("a stored 4xx was not replayed")
	}

	// Batches and jobs replay too, so a retried client stores nothing twice.
	patient := s.patient(t, "idem-batch")
	batch := map[string]any{"items": s.heartRateSeries(patient, "e2e-monitor")}
	bk := map[string]string{"Idempotency-Key": key + "-batch"}
	s.do(t, http.MethodPost, "/api/v1/measurements/batch", s.operator.token, batch, bk).expect(t, http.StatusCreated, "")
	br := s.do(t, http.MethodPost, "/api/v1/measurements/batch", s.operator.token, batch, bk).expect(t, http.StatusCreated, "")
	if br.header.Get("Idempotency-Replayed") != "true" {
		t.Error("batch replay not marked")
	}
	if n := s.count(t, "SELECT count(*) FROM measurements WHERE patient_id = $1", patient); n != 60 {
		t.Errorf("%d readings after a replayed batch, want 60", n)
	}
	jk := map[string]string{"Idempotency-Key": key + "-job"}
	j1 := s.do(t, http.MethodPost, "/api/v1/processing/jobs", s.operator.token, s.jobRequest(patient), jk).expect(t, http.StatusCreated, "")
	j2 := s.do(t, http.MethodPost, "/api/v1/processing/jobs", s.operator.token, s.jobRequest(patient), jk).expect(t, http.StatusCreated, "")
	var ja, jb jobView
	j1.decode(t, &ja)
	j2.decode(t, &jb)
	if ja.ID != jb.ID || j2.header.Get("Idempotency-Replayed") != "true" {
		t.Errorf("job replay: %s then %s", ja.ID, jb.ID)
	}
	if n := s.count(t, "SELECT count(*) FROM processing_jobs WHERE patient_id = $1", patient); n != 1 {
		t.Errorf("%d jobs after a replayed create, want 1", n)
	}

	// A malformed key is refused before anything runs.
	s.do(t, http.MethodPost, "/api/v1/patients", s.operator.token, body, map[string]string{"Idempotency-Key": "has spaces,and commas"}).expect(t, http.StatusUnprocessableEntity, "INVALID_IDEMPOTENCY_KEY")
}

// -------------------------------------------------------- audit logging

func (s *stack) testAuditLogging(t *testing.T) {
	login := s.do(t, http.MethodPost, "/api/v1/auth/login", "", map[string]string{"email": s.operator.email, "password": password}, nil).expect(t, http.StatusOK, "")
	if n := s.count(t, "SELECT count(*) FROM audit_logs WHERE action = 'LOGIN' AND actor_id = $1 AND actor_type = 'USER' AND request_id = $2", s.operator.id, login.requestID()); n != 1 {
		t.Errorf("%d LOGIN audit rows for request %s, want 1", n, login.requestID())
	}

	patientResp := s.do(t, http.MethodPost, "/api/v1/patients", s.operator.token, map[string]string{"external_reference": "e2e-" + s.runID + "-audit", "date_of_birth": "1960-12-01", "sex": "OTHER"}, nil).expect(t, http.StatusCreated, "")
	var p struct {
		ID uuid.UUID `json:"id"`
	}
	patientResp.decode(t, &p)
	rows := s.query(t, "SELECT actor_id, actor_type, resource_type, request_id FROM audit_logs WHERE action = 'PATIENT_CREATED' AND resource_id = $1", p.ID)
	if len(rows) != 1 {
		t.Fatalf("%d PATIENT_CREATED rows", len(rows))
	}
	if actor, _ := rows[0]["actor_id"].([16]byte); uuid.UUID(actor) != s.operator.id {
		t.Errorf("PATIENT_CREATED actor = %v, want the operator %s", rows[0]["actor_id"], s.operator.id)
	}
	if rows[0]["actor_type"] != "USER" || rows[0]["resource_type"] != "patient" || rows[0]["request_id"] != patientResp.requestID() {
		t.Errorf("PATIENT_CREATED row = %v (request %s)", rows[0], patientResp.requestID())
	}

	// One record per reading of a batch, under the batch's request id.
	batch := s.do(t, http.MethodPost, "/api/v1/measurements/batch", s.operator.token, map[string]any{"items": s.heartRateSeries(p.ID, "e2e-monitor")}, nil).expect(t, http.StatusCreated, "")
	if n := s.count(t, "SELECT count(*) FROM audit_logs WHERE action = 'MEASUREMENT_CREATED' AND request_id = $1", batch.requestID()); n != 60 {
		t.Errorf("%d MEASUREMENT_CREATED rows for the batch, want 60", n)
	}
	metadata := s.query(t, "SELECT metadata FROM audit_logs WHERE action = 'MEASUREMENT_CREATED' AND request_id = $1 LIMIT 1", batch.requestID())
	if len(metadata) != 1 || !strings.Contains(fmt.Sprint(metadata[0]["metadata"]), p.ID.String()) {
		t.Errorf("a MEASUREMENT_CREATED record does not name the patient: %v", metadata)
	}

	job := s.do(t, http.MethodPost, "/api/v1/processing/jobs", s.operator.token, s.jobRequest(p.ID), nil).expect(t, http.StatusCreated, "")
	var j jobView
	job.decode(t, &j)
	if n := s.count(t, "SELECT count(*) FROM audit_logs WHERE action = 'PROCESSING_JOB_CREATED' AND resource_id = $1 AND request_id = $2", j.ID, job.requestID()); n != 1 {
		t.Errorf("%d PROCESSING_JOB_CREATED rows, want 1", n)
	}

	// Refused requests leave no record, and nothing in a record is a
	// credential or a reading value.
	s.do(t, http.MethodPost, "/api/v1/patients", s.user.token, map[string]string{"external_reference": "e2e-" + s.runID + "-audit-denied", "date_of_birth": "1960-12-01", "sex": "OTHER"}, nil).expect(t, http.StatusForbidden, "PERMISSION_DENIED")
	if n := s.count(t, "SELECT count(*) FROM audit_logs WHERE metadata::text LIKE '%audit-denied%'"); n != 0 {
		t.Error("a denied request was audited as a change")
	}
	if n := s.count(t, "SELECT count(*) FROM audit_logs WHERE metadata::text LIKE $1", "%"+password+"%"); n != 0 {
		t.Error("a password is in the audit log")
	}
	// A reading value must never be audited. Asserting that by searching
	// the metadata for a number is not safe: the metadata carries the
	// patient id, and a random UUID contains any given three hex digits
	// about once in every hundred and forty rows. So this asserts the
	// shape instead -- the metadata carries these two keys and nothing
	// else -- which is stronger as well as stable, because it also
	// catches a field added later.
	if n := s.count(t, `SELECT count(*) FROM audit_logs
		WHERE action = 'MEASUREMENT_CREATED' AND request_id = $1
		  AND NOT (ARRAY(SELECT jsonb_object_keys(metadata)) <@ ARRAY['type', 'patient_id'])`,
		batch.requestID()); n != 0 {
		t.Errorf("%d MEASUREMENT_CREATED records carry a field beyond the patient and the type; a reading value must never be audited", n)
	}

	// Append-only at the database: no role can rewrite history.
	if _, err := s.db.Exec(t.Context(), "UPDATE audit_logs SET request_id = 'tampered' WHERE resource_id = $1", p.ID); err == nil {
		t.Error("an UPDATE of audit_logs succeeded")
	}
	if _, err := s.db.Exec(t.Context(), "DELETE FROM audit_logs WHERE resource_id = $1", p.ID); err == nil {
		t.Error("a DELETE from audit_logs succeeded")
	}
}

// -------------------------------------------------------- rate limiting

func (s *stack) testRateLimiting(t *testing.T) {
	patient := s.patient(t, "ratelimit")
	path := "/api/v1/patients/" + patient.String()

	first := s.do(t, http.MethodGet, path, s.limited.token, nil, nil).expect(t, http.StatusOK, "")
	limit, err := strconv.Atoi(first.header.Get("RateLimit-Limit"))
	if err != nil || limit < 1 {
		t.Fatalf("RateLimit-Limit = %q", first.header.Get("RateLimit-Limit"))
	}
	remaining, _ := strconv.Atoi(first.header.Get("RateLimit-Remaining"))
	if first.header.Get("RateLimit-Reset") == "" {
		t.Error("no RateLimit-Reset header")
	}
	// The account signed in once (anonymous budget) and made one request;
	// the rest of the window is ours to spend.
	if remaining >= limit || remaining < limit-2 {
		t.Errorf("RateLimit-Remaining = %d with limit %d after one request", remaining, limit)
	}

	var refused response
	for i := 0; i < remaining+2; i++ {
		r := s.do(t, http.MethodGet, path, s.limited.token, nil, nil)
		if r.status == http.StatusTooManyRequests {
			refused = r
			break
		}
		if r.status != http.StatusOK {
			t.Fatalf("request %d: %s", i, r.describe())
		}
	}
	if refused.status != http.StatusTooManyRequests {
		t.Fatalf("no 429 after spending the whole budget of %d", limit)
	}
	refused.expect(t, http.StatusTooManyRequests, "RATE_LIMIT_EXCEEDED")
	if ra, err := strconv.Atoi(refused.header.Get("Retry-After")); err != nil || ra < 1 || ra > 60 {
		t.Errorf("Retry-After = %q, want seconds within the window", refused.header.Get("Retry-After"))
	}
	if refused.header.Get("RateLimit-Remaining") != "0" {
		t.Errorf("RateLimit-Remaining = %q on refusal", refused.header.Get("RateLimit-Remaining"))
	}

	// The budget is the account's: the operator is not affected, and the
	// health endpoints are never counted.
	s.do(t, http.MethodGet, path, s.operator.token, nil, nil).expect(t, http.StatusOK, "")
	for i := 0; i < 3; i++ {
		s.do(t, http.MethodGet, "/health", "", nil, nil).expect(t, http.StatusOK, "")
	}
}
