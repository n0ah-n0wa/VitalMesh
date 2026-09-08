package httpapi

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
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
	"github.com/n0ah-n0wa/VitalMesh/services/api-gateway/internal/idempotency/idempotencytest"
	"github.com/n0ah-n0wa/VitalMesh/services/api-gateway/internal/observability/logging"
	"github.com/n0ah-n0wa/VitalMesh/services/api-gateway/internal/observability/metrics"
	"github.com/n0ah-n0wa/VitalMesh/services/api-gateway/internal/patient"
	"github.com/n0ah-n0wa/VitalMesh/services/api-gateway/internal/patient/patienttest"
)

// API tests: the Patient API through NewHandler, so the real router,
// middleware chain, authentication, authorization table and handlers are
// all exercised; only the store is in memory.

var patientsNow = time.Date(2026, 9, 6, 12, 0, 0, 0, time.UTC)

type httpMetrics struct {
	requests []string
}

func (m *httpMetrics) HTTPRequest(method, route string, status int, _ time.Duration) {
	m.requests = append(m.requests, fmt.Sprintf("%s %s %d", method, route, status))
}
func (m *httpMetrics) Operation(string, metrics.Outcome, time.Duration) {}
func (m *httpMetrics) Batch(string, int)                                {}
func (m *httpMetrics) Cache(string, bool)                               {}

type patientsAPI struct {
	handler http.Handler
	store   *patienttest.MemoryStore
	tokens  *auth.Tokens
	logs    *bytes.Buffer
	metrics *httpMetrics
}

func newPatientsAPI(t *testing.T) *patientsAPI {
	t.Helper()
	var logBuf bytes.Buffer
	logger := logging.New(&logBuf, config.Log{Format: config.LogFormatJSON}, logging.Service{Name: "test"})
	tokens := auth.NewTokens(routesJWT(routesSecret), func() time.Time { return patientsNow })
	store := patienttest.NewMemoryStore()
	service := patient.NewService(store, logger, patient.Options{Now: func() time.Time { return patientsNow }})
	rec := &httpMetrics{}

	h, err := NewHandler(config.HTTP{MaxBodyBytes: 1 << 20, RequestTimeout: time.Second}, logger, Handlers{
		Health:       handler.NewHealth("test", "v", health.NewReadiness(time.Second), logger),
		Patients:     handler.NewPatients(service, logger),
		Authenticate: middleware.Authenticate(tokens, logger),
		Idempotency:  middleware.Idempotency(idempotencytest.NewMemoryStore(), time.Hour, nil, logger),
		Policy:       authz.Default(),
		Metrics:      rec,
	})
	if err != nil {
		t.Fatalf("NewHandler: %v", err)
	}
	return &patientsAPI{handler: h, store: store, tokens: tokens, logs: &logBuf, metrics: rec}
}

func (a *patientsAPI) token(t *testing.T, role domain.Role) string {
	t.Helper()
	token, _, err := a.tokens.Issue(uuid.New(), role)
	if err != nil {
		t.Fatal(err)
	}
	return "Bearer " + token
}

func (a *patientsAPI) do(method, path, body, authorization string) *httptest.ResponseRecorder {
	var rd *strings.Reader
	if body != "" {
		rd = strings.NewReader(body)
	}
	var req *http.Request
	if rd != nil {
		req = httptest.NewRequest(method, path, rd)
	} else {
		req = httptest.NewRequest(method, path, nil)
	}
	if method == http.MethodPost {
		req.Header.Set("Content-Type", "application/json")
	}
	if authorization != "" {
		req.Header.Set("Authorization", authorization)
	}
	rec := httptest.NewRecorder()
	a.handler.ServeHTTP(rec, req)
	return rec
}

const validPatient = `{"external_reference":"synthetic-0001","date_of_birth":"1984-02-29","sex":"FEMALE"}`

func decodePatient(t *testing.T, rec *httptest.ResponseRecorder) model.Patient {
	t.Helper()
	var p model.Patient
	if err := json.NewDecoder(rec.Body).Decode(&p); err != nil {
		t.Fatalf("decode patient: %v (%s)", err, rec.Body.String())
	}
	return p
}

func TestPatientCreateSucceeds(t *testing.T) {
	api := newPatientsAPI(t)
	rec := api.do(http.MethodPost, "/api/v1/patients", validPatient, api.token(t, domain.RoleOperator))
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d (%s)", rec.Code, rec.Body.String())
	}
	p := decodePatient(t, rec)
	if p.ID == uuid.Nil || p.ExternalReference != "synthetic-0001" || p.DateOfBirth != "1984-02-29" || p.Sex != "FEMALE" || p.Status != "ACTIVE" || p.CreatedAt.IsZero() {
		t.Errorf("patient = %+v", p)
	}
	if got := rec.Header().Get("Location"); got != "/api/v1/patients/"+p.ID.String() {
		t.Errorf("Location = %q", got)
	}
	if strings.Contains(rec.Body.String(), "deleted_at") {
		t.Error("response exposes internal columns")
	}
	if len(api.store.Audit) != 1 || api.store.Audit[0].Event.Action != domain.AuditPatientCreated || api.store.Audit[0].Event.RequestID == "" {
		t.Errorf("audit = %+v", api.store.Audit)
	}
	if api.metrics.requests[len(api.metrics.requests)-1] != "POST POST /api/v1/patients 201" {
		t.Errorf("metrics = %v", api.metrics.requests)
	}
	if !strings.Contains(api.logs.String(), `"route":"POST /api/v1/patients"`) {
		t.Errorf("route not logged: %s", api.logs.String())
	}
}

func TestPatientCreateValidationFailures(t *testing.T) {
	api := newPatientsAPI(t)
	token := api.token(t, domain.RoleAdmin)
	cases := map[string]struct {
		body   string
		status int
		code   string
		field  string
	}{
		"empty body":         {"", http.StatusBadRequest, "EMPTY_BODY", ""},
		"malformed":          {`{"external_reference":`, http.StatusBadRequest, "INVALID_JSON", ""},
		"unknown field":      {`{"external_reference":"x","date_of_birth":"1984-02-29","sex":"MALE","name":"Jo"}`, http.StatusBadRequest, "INVALID_JSON", "name"},
		"missing fields":     {`{}`, http.StatusUnprocessableEntity, "VALIDATION_FAILED", "external_reference"},
		"bad date":           {`{"external_reference":"x","date_of_birth":"29/02/1984","sex":"MALE"}`, http.StatusUnprocessableEntity, "VALIDATION_FAILED", "date_of_birth"},
		"future date":        {`{"external_reference":"x","date_of_birth":"2030-01-01","sex":"MALE"}`, http.StatusUnprocessableEntity, "VALIDATION_FAILED", "date_of_birth"},
		"bad sex":            {`{"external_reference":"x","date_of_birth":"1984-02-29","sex":"M"}`, http.StatusUnprocessableEntity, "VALIDATION_FAILED", "sex"},
		"reference too long": {`{"external_reference":"` + strings.Repeat("r", 129) + `","date_of_birth":"1984-02-29","sex":"MALE"}`, http.StatusUnprocessableEntity, "VALIDATION_FAILED", "external_reference"},
		"wrong type":         {`{"external_reference":1,"date_of_birth":"1984-02-29","sex":"MALE"}`, http.StatusBadRequest, "INVALID_JSON", "external_reference"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			rec := api.do(http.MethodPost, "/api/v1/patients", tc.body, token)
			if rec.Code != tc.status {
				t.Fatalf("status = %d, want %d (%s)", rec.Code, tc.status, rec.Body.String())
			}
			e := envelope(t, rec)
			if e.Code != tc.code {
				t.Errorf("code = %q, want %q", e.Code, tc.code)
			}
			if tc.field != "" {
				found := false
				for _, d := range e.Details {
					if d.Field == tc.field {
						found = true
					}
				}
				if !found {
					t.Errorf("details = %+v, want field %s", e.Details, tc.field)
				}
			}
		})
	}
	if len(api.store.Audit) != 0 {
		t.Errorf("rejected requests were audited: %+v", api.store.Audit)
	}

	rec := api.do(http.MethodPost, "/api/v1/patients", validPatient, token)
	if rec.Code != http.StatusCreated {
		t.Fatal(rec.Body.String())
	}
	rec = api.do(http.MethodPost, "/api/v1/patients", validPatient, token)
	if rec.Code != http.StatusConflict || envelope(t, rec).Code != patient.CodeAlreadyExists {
		t.Errorf("duplicate: %d %s", rec.Code, rec.Body.String())
	}
}

func TestPatientEndpointsRequireAuthentication(t *testing.T) {
	api := newPatientsAPI(t)
	id := api.store.Seed(1)[0].ID
	expired := auth.NewTokens(routesJWT(routesSecret), func() time.Time { return patientsNow.Add(-time.Hour) })
	staleAdmin, _, _ := expired.Issue(uuid.New(), domain.RoleAdmin)

	for _, tc := range []struct {
		method, path, auth string
		code               string
	}{
		{http.MethodPost, "/api/v1/patients", "", auth.CodeAuthenticationRequired},
		{http.MethodGet, "/api/v1/patients", "", auth.CodeAuthenticationRequired},
		{http.MethodGet, "/api/v1/patients/" + id.String(), "", auth.CodeAuthenticationRequired},
		{http.MethodDelete, "/api/v1/patients/" + id.String(), "", auth.CodeAuthenticationRequired},
		{http.MethodGet, "/api/v1/patients", "Bearer not.a.token", auth.CodeInvalidToken},
		{http.MethodDelete, "/api/v1/patients/" + id.String(), "Bearer " + staleAdmin, auth.CodeTokenExpired},
	} {
		body := ""
		if tc.method == http.MethodPost {
			body = validPatient
		}
		rec := api.do(tc.method, tc.path, body, tc.auth)
		if rec.Code != http.StatusUnauthorized || envelope(t, rec).Code != tc.code {
			t.Errorf("%s %s: %d %s", tc.method, tc.path, rec.Code, rec.Body.String())
		}
		if rec.Header().Get("WWW-Authenticate") == "" {
			t.Errorf("%s %s: no challenge", tc.method, tc.path)
		}
	}
	if len(api.store.Audit) != 0 {
		t.Error("unauthenticated requests changed state")
	}
	if p, _ := api.store.Get(id); p.Status != domain.PatientActive {
		t.Error("unauthenticated delete took effect")
	}
}

func TestPatientEndpointsEnforceRoles(t *testing.T) {
	api := newPatientsAPI(t)
	id := api.store.Seed(1)[0].ID
	user := api.token(t, domain.RoleUser)

	// USER may read.
	if rec := api.do(http.MethodGet, "/api/v1/patients", "", user); rec.Code != http.StatusOK {
		t.Errorf("USER list: %d %s", rec.Code, rec.Body.String())
	}
	if rec := api.do(http.MethodGet, "/api/v1/patients/"+id.String(), "", user); rec.Code != http.StatusOK {
		t.Errorf("USER get: %d %s", rec.Code, rec.Body.String())
	}
	// USER may not write.
	if rec := api.do(http.MethodPost, "/api/v1/patients", validPatient, user); rec.Code != http.StatusForbidden || envelope(t, rec).Code != authz.CodePermissionDenied {
		t.Errorf("USER create: %d %s", rec.Code, rec.Body.String())
	}
	if rec := api.do(http.MethodDelete, "/api/v1/patients/"+id.String(), "", user); rec.Code != http.StatusForbidden {
		t.Errorf("USER delete: %d %s", rec.Code, rec.Body.String())
	}
	if p, _ := api.store.Get(id); p.Status != domain.PatientActive || len(api.store.Audit) != 0 {
		t.Error("forbidden requests changed state")
	}
	// OPERATOR and ADMIN may write.
	for _, role := range []domain.Role{domain.RoleOperator, domain.RoleAdmin} {
		body := strings.Replace(validPatient, "synthetic-0001", "ref-"+string(role), 1)
		if rec := api.do(http.MethodPost, "/api/v1/patients", body, api.token(t, role)); rec.Code != http.StatusCreated {
			t.Errorf("%s create: %d %s", role, rec.Code, rec.Body.String())
		}
	}
}

func TestPatientGetAndNotFound(t *testing.T) {
	api := newPatientsAPI(t)
	seeded := api.store.Seed(1)[0]
	token := api.token(t, domain.RoleUser)

	rec := api.do(http.MethodGet, "/api/v1/patients/"+seeded.ID.String(), "", token)
	if rec.Code != http.StatusOK {
		t.Fatalf("get: %d %s", rec.Code, rec.Body.String())
	}
	if p := decodePatient(t, rec); p.ID != seeded.ID || p.ExternalReference != seeded.ExternalReference || p.DateOfBirth != "1980-01-01" {
		t.Errorf("patient = %+v", p)
	}

	for _, path := range []string{
		"/api/v1/patients/" + uuid.NewString(),
		"/api/v1/patients/not-a-uuid",
		"/api/v1/patients/00000000-0000-0000-0000-000000000000",
	} {
		rec := api.do(http.MethodGet, path, "", token)
		if rec.Code != http.StatusNotFound || envelope(t, rec).Code != patient.CodeNotFound {
			t.Errorf("GET %s: %d %s", path, rec.Code, rec.Body.String())
		}
		rec = api.do(http.MethodDelete, path, "", api.token(t, domain.RoleAdmin))
		if rec.Code != http.StatusNotFound || envelope(t, rec).Code != patient.CodeNotFound {
			t.Errorf("DELETE %s: %d %s", path, rec.Code, rec.Body.String())
		}
	}
}

func TestPatientDeletion(t *testing.T) {
	api := newPatientsAPI(t)
	id := api.store.Seed(1)[0].ID
	operator := api.token(t, domain.RoleOperator)

	rec := api.do(http.MethodDelete, "/api/v1/patients/"+id.String(), "", operator)
	if rec.Code != http.StatusNoContent || rec.Body.Len() != 0 {
		t.Fatalf("delete: %d %q", rec.Code, rec.Body.String())
	}
	if len(api.store.Audit) != 1 || api.store.Audit[0].Event.Action != domain.AuditPatientDeleted {
		t.Errorf("audit = %+v", api.store.Audit)
	}

	// Gone for non-admins, visible with its status for ADMIN, absent from
	// the list for everyone.
	if rec := api.do(http.MethodGet, "/api/v1/patients/"+id.String(), "", operator); rec.Code != http.StatusNotFound {
		t.Errorf("OPERATOR get after delete: %d", rec.Code)
	}
	rec = api.do(http.MethodGet, "/api/v1/patients/"+id.String(), "", api.token(t, domain.RoleAdmin))
	if rec.Code != http.StatusOK || decodePatient(t, rec).Status != "DELETED" {
		t.Errorf("ADMIN get after delete: %d %s", rec.Code, rec.Body.String())
	}
	rec = api.do(http.MethodGet, "/api/v1/patients", "", operator)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"items":[]`) {
		t.Errorf("list after delete: %d %s", rec.Code, rec.Body.String())
	}

	// Deleting again is a conflict, not a silent success.
	rec = api.do(http.MethodDelete, "/api/v1/patients/"+id.String(), "", operator)
	if rec.Code != http.StatusConflict || envelope(t, rec).Code != patient.CodeAlreadyDeleted {
		t.Errorf("second delete: %d %s", rec.Code, rec.Body.String())
	}
	if len(api.store.Audit) != 1 {
		t.Errorf("failed delete audited: %+v", api.store.Audit)
	}
}

func TestPatientListPagination(t *testing.T) {
	api := newPatientsAPI(t)
	seeded := api.store.Seed(5)
	token := api.token(t, domain.RoleUser)

	var ids []uuid.UUID
	cursor := ""
	for {
		path := "/api/v1/patients?limit=2"
		if cursor != "" {
			path += "&cursor=" + cursor
		}
		rec := api.do(http.MethodGet, path, "", token)
		if rec.Code != http.StatusOK {
			t.Fatalf("list: %d %s", rec.Code, rec.Body.String())
		}
		var page model.Page[model.Patient]
		if err := json.NewDecoder(rec.Body).Decode(&page); err != nil {
			t.Fatal(err)
		}
		for _, p := range page.Items {
			ids = append(ids, p.ID)
		}
		if !page.HasMore {
			if page.NextCursor != nil {
				t.Error("last page carries a cursor")
			}
			break
		}
		cursor = *page.NextCursor
	}
	if len(ids) != 5 {
		t.Fatalf("walked %d patients, want 5", len(ids))
	}
	for i, p := range seeded {
		if ids[i] != p.ID {
			t.Errorf("item %d = %s, want %s (creation order)", i, ids[i], p.ID)
		}
	}

	for name, query := range map[string]string{
		"limit zero":     "?limit=0",
		"limit too big":  "?limit=201",
		"limit text":     "?limit=ten",
		"bad cursor":     "?cursor=%21%21%21",
		"foreign cursor": "?cursor=eyJwYWdlIjoyfQ",
	} {
		rec := api.do(http.MethodGet, "/api/v1/patients"+query, "", token)
		if rec.Code != http.StatusUnprocessableEntity {
			t.Errorf("%s: status = %d (%s)", name, rec.Code, rec.Body.String())
		}
	}

	rec := api.do(http.MethodGet, "/api/v1/patients", "", token)
	var page model.Page[model.Patient]
	_ = json.NewDecoder(rec.Body).Decode(&page)
	if len(page.Items) != 5 || page.HasMore {
		t.Errorf("default page = %d items, has_more %v", len(page.Items), page.HasMore)
	}
}

func TestPatientMethodNotAllowed(t *testing.T) {
	api := newPatientsAPI(t)
	rec := api.do(http.MethodPut, "/api/v1/patients", "", api.token(t, domain.RoleAdmin))
	if rec.Code != http.StatusMethodNotAllowed || rec.Header().Get("Allow") != "POST, GET" {
		t.Errorf("PUT: %d Allow=%q", rec.Code, rec.Header().Get("Allow"))
	}
	if api.metrics.requests[len(api.metrics.requests)-1] != "PUT unmatched 405" {
		t.Errorf("metrics = %v", api.metrics.requests)
	}
}

func TestPatientCreateHonoursIdempotencyKey(t *testing.T) {
	api := newPatientsAPI(t)
	token := api.token(t, domain.RoleOperator)
	key := map[string]string{"Idempotency-Key": "patient-key-1"}
	doWith := func(body string, headers map[string]string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/api/v1/patients", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", token)
		for k, v := range headers {
			req.Header.Set(k, v)
		}
		rec := httptest.NewRecorder()
		api.handler.ServeHTTP(rec, req)
		return rec
	}
	first := doWith(validPatient, key)
	replay := doWith(validPatient, key)
	if first.Code != http.StatusCreated || replay.Code != http.StatusCreated || replay.Header().Get("Idempotency-Replayed") != "true" || replay.Body.String() != first.Body.String() {
		t.Errorf("first %d, replay %d replayed=%q", first.Code, replay.Code, replay.Header().Get("Idempotency-Replayed"))
	}
	if len(api.store.Audit) != 1 {
		t.Errorf("replay created a second patient: %d audit events", len(api.store.Audit))
	}
	if rec := doWith(strings.Replace(validPatient, "synthetic-0001", "synthetic-0002", 1), key); rec.Code != http.StatusUnprocessableEntity {
		t.Errorf("reused key with another body: %d", rec.Code)
	}
	if rec := doWith(validPatient, nil); rec.Code != http.StatusConflict {
		t.Errorf("same patient without key: %d, want 409 from the unique reference", rec.Code)
	}
}
