//go:build integration

package app

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/n0ah-n0wa/VitalMesh/services/api-gateway/internal/auth"
	"github.com/n0ah-n0wa/VitalMesh/services/api-gateway/internal/config"
	"github.com/n0ah-n0wa/VitalMesh/services/api-gateway/internal/domain"
	"github.com/n0ah-n0wa/VitalMesh/services/api-gateway/internal/httpapi/middleware"
	"github.com/n0ah-n0wa/VitalMesh/services/api-gateway/internal/httpapi/model"
	"github.com/n0ah-n0wa/VitalMesh/services/api-gateway/internal/idempotency"
	"github.com/n0ah-n0wa/VitalMesh/services/api-gateway/internal/infra/postgres"
	"github.com/n0ah-n0wa/VitalMesh/services/api-gateway/internal/infra/postgres/postgrestest"
	"github.com/n0ah-n0wa/VitalMesh/services/api-gateway/internal/observability/logging"
)

// The Measurement API end to end against PostgreSQL, including the
// idempotency_keys table behind Idempotency-Key.
func TestMeasurementAPIAgainstPostgres(t *testing.T) {
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
		case "MEASUREMENT_MAX_BATCH_SIZE":
			return "3", true
		}
		return "", false
	})
	if err != nil {
		t.Fatalf("config: %v", err)
	}
	var logs bytes.Buffer
	a, err := New(context.Background(), cfg, logging.New(&logs, config.Log{Format: config.LogFormatJSON}, logging.Service{Name: "test"}), "v-int")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer a.pool.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	hash, err := auth.NewHasher(cfg.Auth.Password).Hash(ctx, "integration-password")
	if err != nil {
		t.Fatal(err)
	}
	operator, err := postgres.NewUsers(pool).Create(ctx, "operator@example.com", hash, domain.RoleOperator)
	if err != nil {
		t.Fatal(err)
	}
	patient, err := postgres.NewPatients(pool).Create(ctx, "int-patient", time.Date(1980, 1, 1, 0, 0, 0, 0, time.UTC), domain.SexUnknown)
	if err != nil {
		t.Fatal(err)
	}

	do := func(method, path, body, authorization string, headers map[string]string) *httptest.ResponseRecorder {
		var req *http.Request
		if body != "" {
			req = httptest.NewRequest(method, path, strings.NewReader(body))
			req.Header.Set("Content-Type", "application/json")
		} else {
			req = httptest.NewRequest(method, path, nil)
		}
		if authorization != "" {
			req.Header.Set("Authorization", authorization)
		}
		for k, v := range headers {
			req.Header.Set(k, v)
		}
		rec := httptest.NewRecorder()
		a.Handler().ServeHTTP(rec, req)
		return rec
	}
	rec := do(http.MethodPost, "/api/v1/auth/login", `{"email":"operator@example.com","password":"integration-password"}`, "", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("login: %d %s", rec.Code, rec.Body.String())
	}
	var session model.LoginResponse
	_ = json.NewDecoder(rec.Body).Decode(&session)
	token := "Bearer " + session.AccessToken

	reading := func(minute int, typ, unit string, value float64) string {
		return fmt.Sprintf(`{"patient_id":%q,"type":%q,"value":%g,"unit":%q,"recorded_at":"2026-09-06T11:%02d:00Z","source":"int-device","metadata":{"seq":%d}}`, patient.ID, typ, value, unit, minute, minute)
	}

	// Idempotent create: the second request replays the first response and
	// stores nothing new; a changed body is refused.
	key := map[string]string{idempotency.Header: "int-key-1"}
	first := do(http.MethodPost, "/api/v1/measurements", reading(0, "HEART_RATE", "bpm", 71), token, key)
	if first.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", first.Code, first.Body.String())
	}
	// The stored response lives in a jsonb column, so a replay is the same
	// JSON document, not necessarily the same bytes.
	replay := do(http.MethodPost, "/api/v1/measurements", reading(0, "HEART_RATE", "bpm", 71), token, key)
	var firstDoc, replayDoc map[string]any
	_ = json.Unmarshal(first.Body.Bytes(), &firstDoc)
	_ = json.Unmarshal(replay.Body.Bytes(), &replayDoc)
	if replay.Code != http.StatusCreated || replay.Header().Get(middleware.ReplayedHeader) != "true" || fmt.Sprint(firstDoc) != fmt.Sprint(replayDoc) || firstDoc["id"] == nil {
		t.Errorf("replay: %d replayed=%q first=%v replay=%v", replay.Code, replay.Header().Get(middleware.ReplayedHeader), firstDoc, replayDoc)
	}
	if reused := do(http.MethodPost, "/api/v1/measurements", reading(0, "HEART_RATE", "bpm", 99), token, key); reused.Code != http.StatusUnprocessableEntity {
		t.Errorf("reused key: %d %s", reused.Code, reused.Body.String())
	}
	var created model.Measurement
	_ = json.NewDecoder(strings.NewReader(first.Body.String())).Decode(&created)

	// Every catalogue type round-trips through the database trigger.
	for i, tc := range []struct {
		typ, unit string
		value     float64
	}{
		{"BLOOD_PRESSURE_SYSTOLIC", "mmHg", 120}, {"BLOOD_PRESSURE_DIASTOLIC", "mmHg", 80}, {"SPO2", "%", 97},
		{"BODY_TEMPERATURE", "C", 36.6}, {"BLOOD_GLUCOSE", "mg/dL", 95}, {"RESPIRATORY_RATE", "breaths/min", 14},
	} {
		if rec := do(http.MethodPost, "/api/v1/measurements", reading(10+i, tc.typ, tc.unit, tc.value), token, nil); rec.Code != http.StatusCreated {
			t.Errorf("%s: %d %s", tc.typ, rec.Code, rec.Body.String())
		}
	}

	// Batch: at the configured limit, then above it, then with a duplicate.
	batch := func(readings ...string) string { return `{"items":[` + strings.Join(readings, ",") + `]}` }
	if rec := do(http.MethodPost, "/api/v1/measurements/batch", batch(reading(20, "HEART_RATE", "bpm", 60), reading(21, "HEART_RATE", "bpm", 61), reading(22, "HEART_RATE", "bpm", 62)), token, nil); rec.Code != http.StatusCreated {
		t.Errorf("batch: %d %s", rec.Code, rec.Body.String())
	}
	if rec := do(http.MethodPost, "/api/v1/measurements/batch", batch(reading(30, "HEART_RATE", "bpm", 60), reading(31, "HEART_RATE", "bpm", 61), reading(32, "HEART_RATE", "bpm", 62), reading(33, "HEART_RATE", "bpm", 63)), token, nil); rec.Code != http.StatusUnprocessableEntity {
		t.Errorf("oversized batch: %d %s", rec.Code, rec.Body.String())
	}
	rec = do(http.MethodPost, "/api/v1/measurements/batch", batch(reading(40, "HEART_RATE", "bpm", 60), reading(21, "HEART_RATE", "bpm", 61)), token, nil)
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), `"items[1]"`) {
		t.Errorf("duplicate batch: %d %s", rec.Code, rec.Body.String())
	}

	// List with a type filter and pagination, then delete.
	rec = do(http.MethodGet, "/api/v1/patients/"+patient.ID.String()+"/measurements?type=HEART_RATE&limit=2", "", token, nil)
	var page model.Page[model.Measurement]
	_ = json.NewDecoder(rec.Body).Decode(&page)
	if rec.Code != http.StatusOK || len(page.Items) != 2 || !page.HasMore || page.Items[0].ID != created.ID {
		t.Errorf("list: %d %s", rec.Code, rec.Body.String())
	}
	// Four HEART_RATE readings exist (one single, three from the batch), so
	// the second page of two is the last.
	rec = do(http.MethodGet, "/api/v1/patients/"+patient.ID.String()+"/measurements?type=HEART_RATE&limit=2&cursor="+*page.NextCursor, "", token, nil)
	second := rec.Body.String()
	page = model.Page[model.Measurement]{}
	_ = json.NewDecoder(strings.NewReader(second)).Decode(&page)
	if rec.Code != http.StatusOK || len(page.Items) != 2 || page.HasMore || page.Items[0].RecordedAt.Minute() != 21 {
		t.Errorf("second page: %d %s", rec.Code, second)
	}
	if rec := do(http.MethodDelete, "/api/v1/measurements/"+created.ID.String(), "", token, map[string]string{"X-Request-ID": "req-del-int"}); rec.Code != http.StatusNoContent {
		t.Errorf("delete: %d %s", rec.Code, rec.Body.String())
	}

	// Audit: created and deleted, by the operator, with the request id.
	entries, err := postgres.NewAudit(pool).ListByResource(ctx, created.ID, 10)
	if err != nil || len(entries) != 2 || entries[0].Action != domain.AuditMeasurementDeleted || entries[0].RequestID != "req-del-int" || *entries[0].ActorID != operator.ID || entries[1].Action != domain.AuditMeasurementCreated {
		t.Errorf("audit = %+v, %v", entries, err)
	}
	total := 0
	for _, action := range []domain.AuditAction{domain.AuditMeasurementCreated} {
		var n int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM audit_logs WHERE action = $1 AND resource_type = 'measurement'`, action).Scan(&n); err != nil {
			t.Fatal(err)
		}
		total += n
	}
	if total != 10 { // 1 + 6 + 3
		t.Errorf("MEASUREMENT_CREATED audit rows = %d, want 10", total)
	}
	var keys int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM idempotency_keys WHERE status = 'COMPLETED'`).Scan(&keys); err != nil || keys != 1 {
		t.Errorf("completed idempotency records = %d, %v", keys, err)
	}
	if strings.Contains(logs.String(), `"seq"`) {
		t.Error("logs contain measurement payload contents")
	}
	_ = uuid.Nil
}
