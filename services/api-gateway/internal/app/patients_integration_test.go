//go:build integration

package app

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/n0ah-n0wa/VitalMesh/services/api-gateway/internal/auth"
	"github.com/n0ah-n0wa/VitalMesh/services/api-gateway/internal/config"
	"github.com/n0ah-n0wa/VitalMesh/services/api-gateway/internal/domain"
	"github.com/n0ah-n0wa/VitalMesh/services/api-gateway/internal/httpapi/model"
	"github.com/n0ah-n0wa/VitalMesh/services/api-gateway/internal/infra/postgres"
	"github.com/n0ah-n0wa/VitalMesh/services/api-gateway/internal/infra/postgres/postgrestest"
	"github.com/n0ah-n0wa/VitalMesh/services/api-gateway/internal/observability/logging"
)

// The Patient API end to end: real PostgreSQL, real login, the wired route
// table and every middleware.
func TestPatientAPIAgainstPostgres(t *testing.T) {
	pool, dbURL, _ := postgrestest.New(t)
	cfg, err := config.Load(func(key string) (string, bool) {
		switch key {
		case "DATABASE_URL":
			return dbURL, true
		case "JWT_SECRET":
			return "integration-secret-integration-secret", true
		case "PASSWORD_HASH_MEMORY_KIB":
			return "8192", true
		case "PASSWORD_HASH_TIME":
			return "1", true
		}
		return "", false
	})
	if err != nil {
		t.Fatalf("config: %v", err)
	}
	var logs bytes.Buffer
	logger := logging.New(&logs, config.Log{Format: config.LogFormatJSON}, logging.Service{Name: "test"})
	a, err := New(context.Background(), cfg, logger, "v-int")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer a.pool.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	hasher := auth.NewHasher(cfg.Auth.Password)
	login := func(email string, role domain.Role) (string, uuid.UUID) {
		hash, err := hasher.Hash(ctx, "integration-password")
		if err != nil {
			t.Fatal(err)
		}
		user, err := postgres.NewUsers(pool).Create(ctx, email, hash, role)
		if err != nil {
			t.Fatal(err)
		}
		req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/login", strings.NewReader(`{"email":"`+email+`","password":"integration-password"}`))
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		a.Handler().ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("login %s: %d %s", email, rec.Code, rec.Body.String())
		}
		var session model.LoginResponse
		_ = json.NewDecoder(rec.Body).Decode(&session)
		return "Bearer " + session.AccessToken, user.ID
	}
	operator, operatorID := login("operator@example.com", domain.RoleOperator)
	user, _ := login("user@example.com", domain.RoleUser)

	do := func(method, path, body, authorization, requestID string) *httptest.ResponseRecorder {
		var req *http.Request
		if body != "" {
			req = httptest.NewRequest(method, path, strings.NewReader(body))
			req.Header.Set("Content-Type", "application/json")
		} else {
			req = httptest.NewRequest(method, path, nil)
		}
		req.Header.Set("Authorization", authorization)
		if requestID != "" {
			req.Header.Set("X-Request-ID", requestID)
		}
		rec := httptest.NewRecorder()
		a.Handler().ServeHTTP(rec, req)
		return rec
	}

	// Create.
	rec := do(http.MethodPost, "/api/v1/patients", `{"external_reference":"int-0001","date_of_birth":"1970-06-15","sex":"OTHER"}`, operator, "req-create-1")
	if rec.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", rec.Code, rec.Body.String())
	}
	var created model.Patient
	_ = json.NewDecoder(rec.Body).Decode(&created)
	if created.DateOfBirth != "1970-06-15" || created.Status != "ACTIVE" || created.Sex != "OTHER" {
		t.Errorf("created = %+v", created)
	}

	// Duplicate, forbidden, read, list.
	if rec := do(http.MethodPost, "/api/v1/patients", `{"external_reference":"int-0001","date_of_birth":"1970-06-15","sex":"OTHER"}`, operator, ""); rec.Code != http.StatusConflict {
		t.Errorf("duplicate: %d %s", rec.Code, rec.Body.String())
	}
	if rec := do(http.MethodPost, "/api/v1/patients", `{"external_reference":"int-0002","date_of_birth":"1970-06-15","sex":"OTHER"}`, user, ""); rec.Code != http.StatusForbidden {
		t.Errorf("USER create: %d %s", rec.Code, rec.Body.String())
	}
	if rec := do(http.MethodGet, "/api/v1/patients/"+created.ID.String(), "", user, ""); rec.Code != http.StatusOK {
		t.Errorf("USER get: %d %s", rec.Code, rec.Body.String())
	}
	rec = do(http.MethodGet, "/api/v1/patients?limit=1", "", user, "")
	var page model.Page[model.Patient]
	_ = json.NewDecoder(rec.Body).Decode(&page)
	if rec.Code != http.StatusOK || len(page.Items) != 1 || page.Items[0].ID != created.ID || page.HasMore {
		t.Errorf("list: %d %s", rec.Code, rec.Body.String())
	}

	// Delete, then visibility.
	if rec := do(http.MethodDelete, "/api/v1/patients/"+created.ID.String(), "", user, ""); rec.Code != http.StatusForbidden {
		t.Errorf("USER delete: %d", rec.Code)
	}
	if rec := do(http.MethodDelete, "/api/v1/patients/"+created.ID.String(), "", operator, "req-delete-1"); rec.Code != http.StatusNoContent {
		t.Errorf("delete: %d %s", rec.Code, rec.Body.String())
	}
	if rec := do(http.MethodGet, "/api/v1/patients/"+created.ID.String(), "", operator, ""); rec.Code != http.StatusNotFound {
		t.Errorf("get after delete: %d", rec.Code)
	}
	if rec := do(http.MethodDelete, "/api/v1/patients/"+created.ID.String(), "", operator, ""); rec.Code != http.StatusConflict {
		t.Errorf("second delete: %d %s", rec.Code, rec.Body.String())
	}

	// Audit trail: exactly one created and one deleted entry, by the
	// operator, under the request ids the client sent.
	entries, err := postgres.NewAudit(pool).ListByResource(ctx, created.ID, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 {
		t.Fatalf("audit entries = %+v", entries)
	}
	if entries[0].Action != domain.AuditPatientDeleted || entries[0].RequestID != "req-delete-1" || *entries[0].ActorID != operatorID ||
		entries[1].Action != domain.AuditPatientCreated || entries[1].RequestID != "req-create-1" || *entries[1].ActorID != operatorID {
		t.Errorf("audit entries = %+v", entries)
	}

	// Structured logs carry the operation, request id and the route.
	for _, want := range []string{`"message":"patient created"`, `"request_id":"req-create-1"`, `"message":"patient deleted"`, `"route":"DELETE /api/v1/patients/{patient_id}"`} {
		if !strings.Contains(logs.String(), want) {
			t.Errorf("logs lack %s", want)
		}
	}
	if strings.Contains(logs.String(), "integration-password") {
		t.Error("logs contain a password")
	}
}
