package httpapi

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/n0ah-n0wa/VitalMesh/services/api-gateway/internal/auth"
	"github.com/n0ah-n0wa/VitalMesh/services/api-gateway/internal/authz"
	"github.com/n0ah-n0wa/VitalMesh/services/api-gateway/internal/config"
	"github.com/n0ah-n0wa/VitalMesh/services/api-gateway/internal/domain"
	"github.com/n0ah-n0wa/VitalMesh/services/api-gateway/internal/health"
	"github.com/n0ah-n0wa/VitalMesh/services/api-gateway/internal/httpapi/handler"
	"github.com/n0ah-n0wa/VitalMesh/services/api-gateway/internal/httpapi/middleware"
	"github.com/n0ah-n0wa/VitalMesh/services/api-gateway/internal/httpapi/model"
	"github.com/n0ah-n0wa/VitalMesh/services/api-gateway/internal/idempotency"
	"github.com/n0ah-n0wa/VitalMesh/services/api-gateway/internal/idempotency/idempotencytest"
	"github.com/n0ah-n0wa/VitalMesh/services/api-gateway/internal/measurement"
	"github.com/n0ah-n0wa/VitalMesh/services/api-gateway/internal/measurement/measurementtest"
	"github.com/n0ah-n0wa/VitalMesh/services/api-gateway/internal/observability/logging"
)

// API tests for the Measurement API through NewHandler: real router,
// middleware chain, authentication, authorization and idempotency; the
// stores are in memory.

const apiBodyLimit = 8 << 10

type measurementsAPI struct {
	handler     http.Handler
	store       *measurementtest.MemoryStore
	idempotency *idempotencytest.MemoryStore
	tokens      *auth.Tokens
	patient     uuid.UUID
	userID      uuid.UUID
}

func newMeasurementsAPI(t *testing.T) *measurementsAPI {
	t.Helper()
	logger := logging.New(io.Discard, config.Log{Format: config.LogFormatJSON}, logging.Service{Name: "test"})
	tokens := auth.NewTokens(routesJWT(routesSecret), func() time.Time { return patientsNow })
	store := measurementtest.NewMemoryStore()
	idem := idempotencytest.NewMemoryStore()
	limits := config.Measurements{MaxBatchSize: 4, MaxMetadataBytes: 256, MaxFutureSkew: 5 * time.Minute}
	service := measurement.NewService(store, limits, logger, measurement.Options{Now: func() time.Time { return patientsNow }})

	h, err := NewHandler(config.HTTP{MaxBodyBytes: apiBodyLimit, RequestTimeout: 2 * time.Second}, logger, Handlers{
		Health:       handler.NewHealth("test", "v", health.NewReadiness(time.Second), logger),
		Measurements: handler.NewMeasurements(service, logger),
		Authenticate: middleware.Authenticate(tokens, logger),
		Idempotency:  middleware.Idempotency(idem, time.Hour, nil, logger),
		Policy:       authz.Default(),
	})
	if err != nil {
		t.Fatalf("NewHandler: %v", err)
	}
	return &measurementsAPI{handler: h, store: store, idempotency: idem, tokens: tokens, patient: store.AddPatient(domain.PatientActive), userID: uuid.New()}
}

func (a *measurementsAPI) token(t *testing.T, role domain.Role) string {
	t.Helper()
	token, _, err := a.tokens.Issue(a.userID, role)
	if err != nil {
		t.Fatal(err)
	}
	return "Bearer " + token
}

func (a *measurementsAPI) do(method, path, body, authorization string, headers map[string]string) *httptest.ResponseRecorder {
	var req *http.Request
	if body != "" {
		req = httptest.NewRequest(method, path, strings.NewReader(body))
	} else {
		req = httptest.NewRequest(method, path, nil)
	}
	if method == http.MethodPost {
		req.Header.Set("Content-Type", "application/json")
	}
	if authorization != "" {
		req.Header.Set("Authorization", authorization)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	a.handler.ServeHTTP(rec, req)
	return rec
}

func (a *measurementsAPI) reading(minute int) string {
	return fmt.Sprintf(`{"patient_id":%q,"type":"HEART_RATE","value":72,"unit":"bpm","recorded_at":"2026-09-06T11:%02d:00Z","source":"monitor-1","metadata":{"lead":"II"}}`, a.patient, minute)
}

func decodeMeasurement(t *testing.T, rec *httptest.ResponseRecorder) model.Measurement {
	t.Helper()
	var m model.Measurement
	if err := json.NewDecoder(rec.Body).Decode(&m); err != nil {
		t.Fatalf("decode: %v (%s)", err, rec.Body.String())
	}
	return m
}

func TestMeasurementCreate(t *testing.T) {
	api := newMeasurementsAPI(t)
	rec := api.do(http.MethodPost, "/api/v1/measurements", api.reading(0), api.token(t, domain.RoleOperator), nil)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d (%s)", rec.Code, rec.Body.String())
	}
	raw := rec.Body.String()
	m := decodeMeasurement(t, rec)
	if m.ID == uuid.Nil || m.PatientID != api.patient || m.Type != "HEART_RATE" || m.Value != 72 || m.Unit != "bpm" || m.Source != "monitor-1" || string(m.Metadata) != `{"lead":"II"}` || m.CreatedAt.IsZero() {
		t.Errorf("measurement = %+v", m)
	}
	if !m.RecordedAt.Equal(time.Date(2026, 9, 6, 11, 0, 0, 0, time.UTC)) {
		t.Errorf("recorded_at = %s", m.RecordedAt)
	}
	if rec.Header().Get("Location") != "/api/v1/measurements/"+m.ID.String() {
		t.Errorf("Location = %q", rec.Header().Get("Location"))
	}
	for _, field := range []string{"id", "patient_id", "type", "value", "unit", "recorded_at", "created_at", "source", "metadata"} {
		if !strings.Contains(raw, `"`+field+`"`) {
			t.Errorf("response lacks %s", field)
		}
	}
	if len(api.store.Audit) != 1 || api.store.Audit[0].Event.Action != domain.AuditMeasurementCreated || api.store.Audit[0].Event.ActorID != api.userID {
		t.Errorf("audit = %+v", api.store.Audit)
	}
}

func TestMeasurementCreateMalformedAndOversized(t *testing.T) {
	api := newMeasurementsAPI(t)
	token := api.token(t, domain.RoleAdmin)
	cases := map[string]struct {
		body   string
		status int
		code   string
	}{
		"empty":            {"", http.StatusBadRequest, "EMPTY_BODY"},
		"truncated":        {`{"patient_id":"x"`, http.StatusBadRequest, "INVALID_JSON"},
		"two values":       {api.reading(0) + api.reading(1), http.StatusBadRequest, "INVALID_JSON"},
		"unknown field":    {strings.Replace(api.reading(0), `"source"`, `"device":"x","source"`, 1), http.StatusBadRequest, "INVALID_JSON"},
		"value as string":  {strings.Replace(api.reading(0), `"value":72`, `"value":"72"`, 1), http.StatusBadRequest, "INVALID_JSON"},
		"value overflow":   {strings.Replace(api.reading(0), `"value":72`, `"value":1e400`, 1), http.StatusBadRequest, "INVALID_JSON"},
		"array body":       {`[` + api.reading(0) + `]`, http.StatusBadRequest, "INVALID_JSON"},
		"missing fields":   {`{}`, http.StatusUnprocessableEntity, measurement.CodeValidationFailed},
		"wrong unit":       {strings.Replace(api.reading(0), `"unit":"bpm"`, `"unit":"mmHg"`, 1), http.StatusUnprocessableEntity, measurement.CodeValidationFailed},
		"out of range":     {strings.Replace(api.reading(0), `"value":72`, `"value":301`, 1), http.StatusUnprocessableEntity, measurement.CodeValidationFailed},
		"bad timestamp":    {strings.Replace(api.reading(0), `2026-09-06T11:00:00Z`, `06/09/2026`, 1), http.StatusUnprocessableEntity, measurement.CodeValidationFailed},
		"unknown patient":  {strings.Replace(api.reading(0), api.patient.String(), uuid.NewString(), 1), http.StatusUnprocessableEntity, measurement.CodeValidationFailed},
		"metadata array":   {strings.Replace(api.reading(0), `{"lead":"II"}`, `[1]`, 1), http.StatusUnprocessableEntity, measurement.CodeValidationFailed},
		"metadata too big": {strings.Replace(api.reading(0), `{"lead":"II"}`, `{"k":"`+strings.Repeat("v", 300)+`"}`, 1), http.StatusUnprocessableEntity, measurement.CodeValidationFailed},
		"oversized body":   {strings.Replace(api.reading(0), `{"lead":"II"}`, `{"k":"`+strings.Repeat("v", apiBodyLimit)+`"}`, 1), http.StatusRequestEntityTooLarge, "REQUEST_BODY_TOO_LARGE"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			rec := api.do(http.MethodPost, "/api/v1/measurements", tc.body, token, nil)
			if rec.Code != tc.status {
				t.Fatalf("status = %d, want %d (%s)", rec.Code, tc.status, rec.Body.String())
			}
			if e := envelope(t, rec); e.Code != tc.code {
				t.Errorf("code = %q, want %q", e.Code, tc.code)
			}
		})
	}
	if api.store.Len() != 0 {
		t.Error("rejected requests stored readings")
	}
}

func TestMeasurementBatch(t *testing.T) {
	api := newMeasurementsAPI(t)
	token := api.token(t, domain.RoleOperator)
	batch := func(readings ...string) string { return `{"items":[` + strings.Join(readings, ",") + `]}` }

	rec := api.do(http.MethodPost, "/api/v1/measurements/batch", batch(api.reading(0), api.reading(1), api.reading(2)), token, nil)
	if rec.Code != http.StatusCreated {
		t.Fatalf("batch: %d %s", rec.Code, rec.Body.String())
	}
	var created model.MeasurementBatch
	_ = json.NewDecoder(rec.Body).Decode(&created)
	if len(created.Items) != 3 || created.Items[2].RecordedAt.Minute() != 2 {
		t.Errorf("created = %+v", created)
	}
	if len(api.store.Audit) != 3 {
		t.Errorf("audit events = %d, want one per reading", len(api.store.Audit))
	}

	// Duplicate of a stored reading: nothing stored, the item is named.
	rec = api.do(http.MethodPost, "/api/v1/measurements/batch", batch(api.reading(5), api.reading(1)), token, nil)
	if rec.Code != http.StatusConflict {
		t.Fatalf("duplicate batch: %d %s", rec.Code, rec.Body.String())
	}
	if e := envelope(t, rec); e.Code != measurement.CodeAlreadyExists || len(e.Details) != 1 || e.Details[0].Field != "items[1]" {
		t.Errorf("envelope = %+v", e)
	}
	if api.store.Len() != 3 {
		t.Errorf("partial batch stored: %d", api.store.Len())
	}

	// Duplicates inside the batch, an invalid item, and oversized batches.
	rec = api.do(http.MethodPost, "/api/v1/measurements/batch", batch(api.reading(7), api.reading(7), strings.Replace(api.reading(8), `"bpm"`, `"BPM"`, 1)), token, nil)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("invalid batch: %d %s", rec.Code, rec.Body.String())
	}
	fields := map[string]bool{}
	for _, d := range envelope(t, rec).Details {
		fields[d.Field] = true
	}
	if !fields["items[1]"] || !fields["items[2].unit"] {
		t.Errorf("details = %v", fields)
	}
	rec = api.do(http.MethodPost, "/api/v1/measurements/batch", batch(api.reading(10), api.reading(11), api.reading(12), api.reading(13), api.reading(14)), token, nil)
	if rec.Code != http.StatusUnprocessableEntity || !strings.Contains(rec.Body.String(), "at most 4") {
		t.Errorf("oversized batch: %d %s", rec.Code, rec.Body.String())
	}
	rec = api.do(http.MethodPost, "/api/v1/measurements/batch", `{"items":[]}`, token, nil)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Errorf("empty batch: %d", rec.Code)
	}
	rec = api.do(http.MethodPost, "/api/v1/measurements/batch", `{}`, token, nil)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Errorf("missing items: %d", rec.Code)
	}
	if api.store.Len() != 3 {
		t.Errorf("invalid batches stored readings: %d", api.store.Len())
	}
}

func TestMeasurementAuthorization(t *testing.T) {
	api := newMeasurementsAPI(t)
	rec := api.do(http.MethodPost, "/api/v1/measurements", api.reading(0), api.token(t, domain.RoleAdmin), nil)
	id := decodeMeasurement(t, rec).ID
	user := api.token(t, domain.RoleUser)

	for _, tc := range []struct {
		method, path, body string
		status             int
	}{
		{http.MethodPost, "/api/v1/measurements", api.reading(1), http.StatusForbidden},
		{http.MethodPost, "/api/v1/measurements/batch", `{"items":[` + api.reading(1) + `]}`, http.StatusForbidden},
		{http.MethodDelete, "/api/v1/measurements/" + id.String(), "", http.StatusForbidden},
		{http.MethodGet, "/api/v1/measurements/" + id.String(), "", http.StatusOK},
		{http.MethodGet, "/api/v1/patients/" + api.patient.String() + "/measurements", "", http.StatusOK},
	} {
		if rec := api.do(tc.method, tc.path, tc.body, user, nil); rec.Code != tc.status {
			t.Errorf("USER %s %s: %d, want %d (%s)", tc.method, tc.path, rec.Code, tc.status, rec.Body.String())
		}
		if rec := api.do(tc.method, tc.path, tc.body, "", nil); rec.Code != http.StatusUnauthorized {
			t.Errorf("anonymous %s %s: %d", tc.method, tc.path, rec.Code)
		}
	}
	if api.store.Len() != 1 {
		t.Error("denied requests changed state")
	}
}

func TestMeasurementGetListDelete(t *testing.T) {
	api := newMeasurementsAPI(t)
	token := api.token(t, domain.RoleOperator)
	var ids []uuid.UUID
	for i := range 5 {
		body := api.reading(i)
		if i%2 == 1 {
			body = strings.Replace(strings.Replace(body, "HEART_RATE", "SPO2", 1), `"unit":"bpm"`, `"unit":"%"`, 1)
		}
		rec := api.do(http.MethodPost, "/api/v1/measurements", body, token, nil)
		if rec.Code != http.StatusCreated {
			t.Fatal(rec.Body.String())
		}
		ids = append(ids, decodeMeasurement(t, rec).ID)
	}

	if rec := api.do(http.MethodGet, "/api/v1/measurements/"+ids[0].String(), "", token, nil); rec.Code != http.StatusOK || decodeMeasurement(t, rec).ID != ids[0] {
		t.Errorf("get: %d %s", rec.Code, rec.Body.String())
	}
	for _, path := range []string{"/api/v1/measurements/" + uuid.NewString(), "/api/v1/measurements/nope"} {
		if rec := api.do(http.MethodGet, path, "", token, nil); rec.Code != http.StatusNotFound || envelope(t, rec).Code != measurement.CodeNotFound {
			t.Errorf("GET %s: %d", path, rec.Code)
		}
	}

	base := "/api/v1/patients/" + api.patient.String() + "/measurements"
	var walked []uuid.UUID
	cursor := ""
	for {
		path := base + "?limit=2"
		if cursor != "" {
			path += "&cursor=" + cursor
		}
		rec := api.do(http.MethodGet, path, "", token, nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("list: %d %s", rec.Code, rec.Body.String())
		}
		var page model.Page[model.Measurement]
		_ = json.NewDecoder(rec.Body).Decode(&page)
		for _, m := range page.Items {
			walked = append(walked, m.ID)
		}
		if !page.HasMore {
			break
		}
		cursor = *page.NextCursor
	}
	if len(walked) != 5 {
		t.Fatalf("walked %d", len(walked))
	}
	for i := range ids {
		if walked[i] != ids[i] {
			t.Errorf("order: %v", walked)
		}
	}
	rec := api.do(http.MethodGet, base+"?type=SPO2&from=2026-09-06T11:01:00Z&to=2026-09-06T11:04:00Z", "", token, nil)
	var page model.Page[model.Measurement]
	_ = json.NewDecoder(rec.Body).Decode(&page)
	if rec.Code != http.StatusOK || len(page.Items) != 2 {
		t.Errorf("filtered: %d %s", rec.Code, rec.Body.String())
	}
	for name, query := range map[string]string{"type": "?type=PULSE", "from": "?from=noon", "range": "?from=2026-09-06T11:04:00Z&to=2026-09-06T11:01:00Z", "limit": "?limit=0", "cursor": "?cursor=%21"} {
		if rec := api.do(http.MethodGet, base+query, "", token, nil); rec.Code != http.StatusUnprocessableEntity {
			t.Errorf("%s: %d (%s)", name, rec.Code, rec.Body.String())
		}
	}
	if rec := api.do(http.MethodGet, "/api/v1/patients/"+uuid.NewString()+"/measurements", "", token, nil); rec.Code != http.StatusNotFound || envelope(t, rec).Code != "PATIENT_NOT_FOUND" {
		t.Errorf("unknown patient list: %d %s", rec.Code, rec.Body.String())
	}

	rec = api.do(http.MethodDelete, "/api/v1/measurements/"+ids[0].String(), "", token, nil)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("delete: %d %s", rec.Code, rec.Body.String())
	}
	if rec := api.do(http.MethodGet, "/api/v1/measurements/"+ids[0].String(), "", token, nil); rec.Code != http.StatusNotFound {
		t.Errorf("get after delete: %d", rec.Code)
	}
	if rec := api.do(http.MethodDelete, "/api/v1/measurements/"+ids[0].String(), "", token, nil); rec.Code != http.StatusNotFound {
		t.Errorf("second delete: %d", rec.Code)
	}
	if last := api.store.Audit[len(api.store.Audit)-1]; last.Event.Action != domain.AuditMeasurementDeleted || last.MeasurementID != ids[0] {
		t.Errorf("delete audit = %+v", last)
	}
}

func TestMeasurementIdempotency(t *testing.T) {
	api := newMeasurementsAPI(t)
	token := api.token(t, domain.RoleOperator)
	key := map[string]string{idempotency.Header: "client-key-0001"}

	first := api.do(http.MethodPost, "/api/v1/measurements", api.reading(0), token, key)
	if first.Code != http.StatusCreated || first.Header().Get(middleware.ReplayedHeader) != "" {
		t.Fatalf("first: %d %s", first.Code, first.Body.String())
	}
	replay := api.do(http.MethodPost, "/api/v1/measurements", api.reading(0), token, key)
	if replay.Code != http.StatusCreated || replay.Header().Get(middleware.ReplayedHeader) != "true" || replay.Body.String() != first.Body.String() {
		t.Errorf("replay: %d replayed=%q body equal=%v", replay.Code, replay.Header().Get(middleware.ReplayedHeader), replay.Body.String() == first.Body.String())
	}
	if replay.Header().Get("Content-Type") != "application/json" {
		t.Errorf("replay Content-Type = %q", replay.Header().Get("Content-Type"))
	}
	if api.store.Len() != 1 || len(api.store.Audit) != 1 {
		t.Errorf("replay changed state: %d readings", api.store.Len())
	}

	// Same key, different body.
	reused := api.do(http.MethodPost, "/api/v1/measurements", api.reading(1), token, key)
	if reused.Code != http.StatusUnprocessableEntity || envelope(t, reused).Code != idempotency.CodeKeyReused {
		t.Errorf("reused key: %d %s", reused.Code, reused.Body.String())
	}
	// Same key on another operation is another scope.
	other := api.do(http.MethodPost, "/api/v1/measurements/batch", `{"items":[`+api.reading(2)+`]}`, token, key)
	if other.Code != http.StatusCreated {
		t.Errorf("same key other path: %d %s", other.Code, other.Body.String())
	}
	// Same key, another account, is another scope too.
	api.userID = uuid.New()
	otherUser := api.do(http.MethodPost, "/api/v1/measurements", api.reading(3), api.token(t, domain.RoleOperator), key)
	if otherUser.Code != http.StatusCreated {
		t.Errorf("same key other user: %d %s", otherUser.Code, otherUser.Body.String())
	}

	// Failed first requests are stored and replayed too (a 4xx is a final
	// answer), but a malformed key is rejected outright.
	bad := api.do(http.MethodPost, "/api/v1/measurements", strings.Replace(api.reading(4), `"bpm"`, `"BPM"`, 1), token, map[string]string{idempotency.Header: "client-key-0002"})
	if bad.Code != http.StatusUnprocessableEntity {
		t.Fatalf("invalid first: %d", bad.Code)
	}
	badReplay := api.do(http.MethodPost, "/api/v1/measurements", strings.Replace(api.reading(4), `"bpm"`, `"BPM"`, 1), token, map[string]string{idempotency.Header: "client-key-0002"})
	if badReplay.Code != http.StatusUnprocessableEntity || badReplay.Header().Get(middleware.ReplayedHeader) != "true" {
		t.Errorf("replay of a 422: %d replayed=%q", badReplay.Code, badReplay.Header().Get(middleware.ReplayedHeader))
	}
	for _, k := range []string{"has space", strings.Repeat("k", 256), "quote\"", "ünïcode"} {
		rec := api.do(http.MethodPost, "/api/v1/measurements", api.reading(6), token, map[string]string{idempotency.Header: k})
		if rec.Code != http.StatusUnprocessableEntity || envelope(t, rec).Code != idempotency.CodeInvalidKey {
			t.Errorf("key %q: %d %s", k, rec.Code, rec.Body.String())
		}
	}
	// Without a key nothing is recorded and duplicates are conflicts.
	before := api.idempotency.Len()
	if rec := api.do(http.MethodPost, "/api/v1/measurements", api.reading(0), token, nil); rec.Code != http.StatusConflict {
		t.Errorf("duplicate without key: %d", rec.Code)
	}
	if api.idempotency.Len() != before {
		t.Error("request without key recorded")
	}
}

func TestMeasurementIdempotencyConcurrentReplays(t *testing.T) {
	api := newMeasurementsAPI(t)
	token := api.token(t, domain.RoleOperator)
	key := map[string]string{idempotency.Header: "burst"}
	body := api.reading(0)

	var wg sync.WaitGroup
	results := make([]int, 8)
	for i := range results {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			results[i] = api.do(http.MethodPost, "/api/v1/measurements", body, token, key).Code
		}(i)
	}
	wg.Wait()
	created := 0
	for _, code := range results {
		switch code {
		case http.StatusCreated:
			created++
		case http.StatusConflict:
		default:
			t.Errorf("unexpected status %d", code)
		}
	}
	if api.store.Len() != 1 {
		t.Errorf("stored %d readings under one key", api.store.Len())
	}
	if created < 1 {
		t.Error("no request succeeded")
	}
}

// bufferedBody is a reader whose length is not declared, exercising the
// undeclared-length path of the body limit through the idempotency read.
func TestMeasurementIdempotencyOversizedUndeclaredBody(t *testing.T) {
	api := newMeasurementsAPI(t)
	big := strings.Replace(api.reading(0), `{"lead":"II"}`, `{"k":"`+strings.Repeat("v", apiBodyLimit)+`"}`, 1)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/measurements", io.NopCloser(bytes.NewBufferString(big)))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", api.token(t, domain.RoleOperator))
	req.Header.Set(idempotency.Header, "big")
	rec := httptest.NewRecorder()
	api.handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("status = %d (%s)", rec.Code, rec.Body.String())
	}
	if api.idempotency.Len() != 0 {
		t.Error("oversized request left an idempotency record")
	}
}

func TestMeasurementOfDeletedPatientIsHiddenFromNonAdmins(t *testing.T) {
	api := newMeasurementsAPI(t)
	rec := api.do(http.MethodPost, "/api/v1/measurements", api.reading(0), api.token(t, domain.RoleOperator), nil)
	id := decodeMeasurement(t, rec).ID
	api.store.Patients[api.patient] = domain.PatientDeleted

	path := "/api/v1/measurements/" + id.String()
	if rec := api.do(http.MethodGet, path, "", api.token(t, domain.RoleUser), nil); rec.Code != http.StatusNotFound {
		t.Errorf("USER get: %d", rec.Code)
	}
	if rec := api.do(http.MethodDelete, path, "", api.token(t, domain.RoleOperator), nil); rec.Code != http.StatusNotFound {
		t.Errorf("OPERATOR delete: %d", rec.Code)
	}
	if rec := api.do(http.MethodGet, "/api/v1/patients/"+api.patient.String()+"/measurements", "", api.token(t, domain.RoleOperator), nil); rec.Code != http.StatusNotFound {
		t.Errorf("OPERATOR list: %d", rec.Code)
	}
	admin := api.token(t, domain.RoleAdmin)
	if rec := api.do(http.MethodGet, path, "", admin, nil); rec.Code != http.StatusOK {
		t.Errorf("ADMIN get: %d", rec.Code)
	}
	if rec := api.do(http.MethodGet, "/api/v1/patients/"+api.patient.String()+"/measurements", "", admin, nil); rec.Code != http.StatusOK {
		t.Errorf("ADMIN list: %d", rec.Code)
	}
	if rec := api.do(http.MethodDelete, path, "", admin, nil); rec.Code != http.StatusNoContent {
		t.Errorf("ADMIN delete: %d", rec.Code)
	}
}
