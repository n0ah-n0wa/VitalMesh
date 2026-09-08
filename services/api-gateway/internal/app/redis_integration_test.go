//go:build integration

package app

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/n0ah-n0wa/VitalMesh/services/api-gateway/internal/auth"
	"github.com/n0ah-n0wa/VitalMesh/services/api-gateway/internal/config"
	"github.com/n0ah-n0wa/VitalMesh/services/api-gateway/internal/domain"
	"github.com/n0ah-n0wa/VitalMesh/services/api-gateway/internal/httpapi/middleware"
	"github.com/n0ah-n0wa/VitalMesh/services/api-gateway/internal/httpapi/model"
	"github.com/n0ah-n0wa/VitalMesh/services/api-gateway/internal/idempotency"
	"github.com/n0ah-n0wa/VitalMesh/services/api-gateway/internal/infra/postgres"
	"github.com/n0ah-n0wa/VitalMesh/services/api-gateway/internal/infra/postgres/postgrestest"
	"github.com/n0ah-n0wa/VitalMesh/services/api-gateway/internal/infra/redisclient/redistest"
	"github.com/n0ah-n0wa/VitalMesh/services/api-gateway/internal/observability/logging"
)

// The whole gateway with Redis behind it, and the same gateway with Redis
// taken away. Every feature Redis serves must work in the first case and
// degrade rather than fail in the second (SPECIFICATIONS.md sections 23, 29,
// 84 and 90).

type redisHarness struct {
	app       *App
	pool      *pgxpool.Pool
	token     string
	adminTok  string
	patientID uuid.UUID
	logs      *bytes.Buffer
	ctx       context.Context
}

// newRedisHarness wires a gateway. redisURL may be empty (no Redis), a live
// address, or an address nothing listens on.
func newRedisHarness(t *testing.T, redisURL string, tune map[string]string) *redisHarness {
	t.Helper()
	pool, dbURL, _ := postgrestest.New(t)

	cfg, err := config.Load(func(key string) (string, bool) {
		if v, ok := tune[key]; ok {
			return v, true
		}
		switch key {
		case "DATABASE_URL":
			return dbURL, true
		case "JWT_SECRET":
			return "redis-integration-secret-redis-integration", true
		case "PASSWORD_HASH_MEMORY_KIB":
			return "8192", true
		case "PASSWORD_HASH_TIME":
			return "1", true
		case "REDIS_URL":
			if redisURL == "" {
				return "", false
			}
			return redisURL, true
		case "REDIS_TIMEOUT":
			return "300ms", true
		case "REDIS_RECOVERY_INTERVAL":
			return "50ms", true
		}
		return "", false
	})
	if err != nil {
		t.Fatalf("config: %v", err)
	}

	logs := &bytes.Buffer{}
	a, err := New(context.Background(), cfg,
		logging.New(logs, config.Log{Format: config.LogFormatJSON}, logging.Service{Name: "test"}), "v-redis")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() {
		a.pool.Close()
		_ = a.redis.Close()
	})

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	t.Cleanup(cancel)

	hash, err := auth.NewHasher(cfg.Auth.Password).Hash(ctx, "redis-integration-password")
	if err != nil {
		t.Fatal(err)
	}
	operator, err := postgres.NewUsers(pool).Create(ctx, "redis-op@example.com", hash, domain.RoleOperator)
	if err != nil {
		t.Fatal(err)
	}
	admin, err := postgres.NewUsers(pool).Create(ctx, "redis-admin@example.com", hash, domain.RoleAdmin)
	if err != nil {
		t.Fatal(err)
	}
	patient, err := postgres.NewPatients(pool).Create(ctx, "redis-patient",
		time.Date(1980, 1, 1, 0, 0, 0, 0, time.UTC), domain.SexUnknown)
	if err != nil {
		t.Fatal(err)
	}

	tokens := auth.NewTokens(cfg.Auth.JWT, nil)
	token, _, err := tokens.Issue(operator.ID, operator.Role)
	if err != nil {
		t.Fatal(err)
	}
	adminToken, _, err := tokens.Issue(admin.ID, admin.Role)
	if err != nil {
		t.Fatal(err)
	}

	return &redisHarness{app: a, pool: pool, token: token, adminTok: adminToken, patientID: patient.ID, logs: logs, ctx: ctx}
}

func (h *redisHarness) do(method, path, body, token string, headers map[string]string) *httptest.ResponseRecorder {
	var req *http.Request
	if body != "" {
		req = httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
	} else {
		req = httptest.NewRequest(method, path, nil)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	h.app.Handler().ServeHTTP(rec, req)
	return rec
}

// ------------------------------------------------- rate limiting

func TestRateLimitingRefusesAnOverBudgetCallerAndSaysWhen(t *testing.T) {
	h := newRedisHarness(t, redistest.URL(t), map[string]string{
		"RATE_LIMIT_AUTHENTICATED": "3",
		"REDIS_NAMESPACE":          "rl-" + strconv.FormatInt(time.Now().UnixNano(), 36),
	})

	path := "/api/v1/patients/" + h.patientID.String()
	for i := 1; i <= 3; i++ {
		rec := h.do(http.MethodGet, path, "", h.token, nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("request %d = %d, want 200: %s", i, rec.Code, rec.Body.String())
		}
		if got := rec.Header().Get(middleware.RateLimitLimitHeader); got != "3" {
			t.Errorf("request %d limit header = %q", i, got)
		}
		if want := strconv.Itoa(3 - i); rec.Header().Get(middleware.RateLimitRemainingHeader) != want {
			t.Errorf("request %d remaining = %q, want %q", i,
				rec.Header().Get(middleware.RateLimitRemainingHeader), want)
		}
	}

	over := h.do(http.MethodGet, path, "", h.token, nil)
	if over.Code != http.StatusTooManyRequests {
		t.Fatalf("the fourth request = %d, want 429: %s", over.Code, over.Body.String())
	}
	var body model.ErrorResponse
	if err := json.Unmarshal(over.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.Error.Code != middleware.CodeRateLimited {
		t.Errorf("code = %s", body.Error.Code)
	}
	retry, err := strconv.Atoi(over.Header().Get("Retry-After"))
	if err != nil || retry < 1 {
		t.Errorf("Retry-After = %q", over.Header().Get("Retry-After"))
	}
	if over.Header().Get(middleware.RateLimitRemainingHeader) != "0" {
		t.Errorf("remaining = %q, want 0", over.Header().Get(middleware.RateLimitRemainingHeader))
	}
}

// A budget belongs to an account, so one caller cannot spend another's.
func TestRateLimitBudgetsAreSeparatePerAccount(t *testing.T) {
	h := newRedisHarness(t, redistest.URL(t), map[string]string{
		"RATE_LIMIT_AUTHENTICATED": "1",
		"RATE_LIMIT_ADMIN":         "5",
		"REDIS_NAMESPACE":          "rl2-" + strconv.FormatInt(time.Now().UnixNano(), 36),
	})
	path := "/api/v1/patients/" + h.patientID.String()

	if rec := h.do(http.MethodGet, path, "", h.token, nil); rec.Code != http.StatusOK {
		t.Fatalf("operator first request = %d", rec.Code)
	}
	if rec := h.do(http.MethodGet, path, "", h.token, nil); rec.Code != http.StatusTooManyRequests {
		t.Fatalf("operator second request = %d, want 429", rec.Code)
	}
	// The administrator has their own, larger budget.
	if rec := h.do(http.MethodGet, path, "", h.adminTok, nil); rec.Code != http.StatusOK {
		t.Errorf("admin request = %d, want 200: the budgets are shared", rec.Code)
	}
}

// The limit must keep applying when Redis is gone, using this replica's own
// counter (OPEN_QUESTIONS OQ-12).
func TestRateLimitingStillAppliesWithoutRedis(t *testing.T) {
	h := newRedisHarness(t, "redis://127.0.0.1:1", map[string]string{
		"RATE_LIMIT_AUTHENTICATED": "2",
	})
	path := "/api/v1/patients/" + h.patientID.String()

	for i := 1; i <= 2; i++ {
		if rec := h.do(http.MethodGet, path, "", h.token, nil); rec.Code != http.StatusOK {
			t.Fatalf("request %d = %d, want 200 while degraded", i, rec.Code)
		}
	}
	if rec := h.do(http.MethodGet, path, "", h.token, nil); rec.Code != http.StatusTooManyRequests {
		t.Errorf("the third request = %d, want 429: the limit stopped applying without Redis", rec.Code)
	}
}

// -------------------------------------------------------- caching

func TestAPatientReadIsServedFromTheCacheAndInvalidatedOnDelete(t *testing.T) {
	h := newRedisHarness(t, redistest.URL(t), map[string]string{
		"REDIS_NAMESPACE": "cache-" + strconv.FormatInt(time.Now().UnixNano(), 36),
	})
	path := "/api/v1/patients/" + h.patientID.String()

	first := h.do(http.MethodGet, path, "", h.adminTok, nil)
	if first.Code != http.StatusOK {
		t.Fatalf("first read = %d: %s", first.Code, first.Body.String())
	}
	second := h.do(http.MethodGet, path, "", h.adminTok, nil)
	if second.Code != http.StatusOK {
		t.Fatalf("second read = %d", second.Code)
	}
	if !sameJSON(t, first.Body.Bytes(), second.Body.Bytes()) {
		t.Error("the cached read returned something different from the first")
	}

	// Deleting the patient must invalidate the entry, not wait for it to
	// expire: an operator must stop seeing the record at once.
	if rec := h.do(http.MethodDelete, path, "", h.adminTok, nil); rec.Code != http.StatusNoContent {
		t.Fatalf("delete = %d: %s", rec.Code, rec.Body.String())
	}
	after := h.do(http.MethodGet, path, "", h.token, nil)
	if after.Code != http.StatusNotFound {
		t.Fatalf("after delete an operator got %d, want 404: a stale cached record was served",
			after.Code)
	}
	// The administrator still sees it, from whichever source, because a
	// deleted patient is visible to that role.
	if rec := h.do(http.MethodGet, path, "", h.adminTok, nil); rec.Code != http.StatusOK {
		t.Errorf("admin read after delete = %d, want 200", rec.Code)
	}
}

// The cache is a read accelerator, never an authority: with it unavailable
// every read still answers, from the database.
func TestReadsKeepWorkingWithoutRedis(t *testing.T) {
	h := newRedisHarness(t, "redis://127.0.0.1:1", nil)
	path := "/api/v1/patients/" + h.patientID.String()

	for i := 0; i < 3; i++ {
		rec := h.do(http.MethodGet, path, "", h.token, nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("read %d = %d, want 200 with Redis gone: %s", i, rec.Code, rec.Body.String())
		}
	}
}

// A cached record must never decide anything. Writing a measurement for a
// deleted patient must be refused even if that patient was read, and so
// cached, moments earlier.
func TestTheCacheNeverDecidesWhetherAWriteIsAllowed(t *testing.T) {
	h := newRedisHarness(t, redistest.URL(t), map[string]string{
		"REDIS_NAMESPACE":   "decide-" + strconv.FormatInt(time.Now().UnixNano(), 36),
		"CACHE_PATIENT_TTL": "5m",
	})
	path := "/api/v1/patients/" + h.patientID.String()

	// Read it, so it is cached as ACTIVE.
	if rec := h.do(http.MethodGet, path, "", h.adminTok, nil); rec.Code != http.StatusOK {
		t.Fatalf("read = %d", rec.Code)
	}
	if rec := h.do(http.MethodDelete, path, "", h.adminTok, nil); rec.Code != http.StatusNoContent {
		t.Fatalf("delete = %d", rec.Code)
	}

	// A write for the deleted patient must be refused. The check behind it
	// goes to the database, never to the cache.
	body := fmt.Sprintf(`{"patient_id":%q,"type":"HEART_RATE","value":72,"unit":"bpm",
		"recorded_at":%q,"source":"test"}`, h.patientID, time.Now().UTC().Format(time.RFC3339))
	rec := h.do(http.MethodPost, "/api/v1/measurements", body, h.token, nil)
	if rec.Code == http.StatusCreated {
		t.Fatal("a measurement was accepted for a deleted patient: a cached record decided a write")
	}
	if rec.Code != http.StatusUnprocessableEntity && rec.Code != http.StatusNotFound {
		t.Errorf("write for a deleted patient = %d: %s", rec.Code, rec.Body.String())
	}
}

// ---------------------------------------------------- idempotency

// The Redis lock is a fast path. Idempotency itself must behave identically
// with it and without it, because PostgreSQL is what serialises replays.
func TestIdempotencyBehavesTheSameWithAndWithoutRedis(t *testing.T) {
	cases := []struct {
		name     string
		redisURL string
	}{
		{"with redis", redistest.URL(t)},
		{"with redis unreachable", "redis://127.0.0.1:1"},
		{"with no redis configured", ""},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newRedisHarness(t, tc.redisURL, map[string]string{
				"REDIS_NAMESPACE": "idem-" + strconv.FormatInt(time.Now().UnixNano(), 36),
			})
			key := map[string]string{idempotency.Header: "redis-test-key"}
			body := `{"external_reference":"idem-` + uuid.NewString() + `","date_of_birth":"1990-01-01","sex":"UNKNOWN"}`

			first := h.do(http.MethodPost, "/api/v1/patients", body, h.token, key)
			if first.Code != http.StatusCreated {
				t.Fatalf("first = %d: %s", first.Code, first.Body.String())
			}
			second := h.do(http.MethodPost, "/api/v1/patients", body, h.token, key)
			if second.Code != http.StatusCreated {
				t.Fatalf("replay = %d: %s", second.Code, second.Body.String())
			}
			if second.Header().Get(middleware.ReplayedHeader) != "true" {
				t.Error("the replay was not served from the record")
			}
			if !sameJSON(t, first.Body.Bytes(), second.Body.Bytes()) {
				t.Errorf("the replay returned a different document; first %s, second %s",
					first.Body.String(), second.Body.String())
			}

			// A different body under the same key is refused, whatever
			// Redis is doing.
			other := `{"external_reference":"idem-other","date_of_birth":"1990-01-01","sex":"UNKNOWN"}`
			reused := h.do(http.MethodPost, "/api/v1/patients", other, h.token, key)
			if reused.Code != http.StatusUnprocessableEntity {
				t.Errorf("reuse with a different body = %d, want 422", reused.Code)
			}
		})
	}
}

// ------------------------------------------------------- readiness

// Redis being down degrades three features and stops none, so a gateway
// without it must stay ready and keep serving (SPECIFICATIONS.md section 90).
func TestTheGatewayStaysReadyAndUsableWithoutRedis(t *testing.T) {
	h := newRedisHarness(t, "redis://127.0.0.1:1", nil)

	ready := h.do(http.MethodGet, "/ready", "", "", nil)
	if ready.Code != http.StatusOK {
		t.Fatalf("/ready = %d, want 200: Redis must not decide readiness: %s", ready.Code, ready.Body.String())
	}
	var report struct {
		Status string `json:"status"`
		Checks []struct {
			Name string `json:"name"`
		} `json:"checks"`
	}
	if err := json.Unmarshal(ready.Body.Bytes(), &report); err != nil {
		t.Fatalf("decode readiness: %v", err)
	}
	for _, check := range report.Checks {
		if check.Name == "redis" {
			t.Error("redis is registered as a required readiness check; its outage would " +
				"take a working gateway out of rotation")
		}
	}
	if live := h.do(http.MethodGet, "/health", "", "", nil); live.Code != http.StatusOK {
		t.Errorf("/health = %d", live.Code)
	}

	// The whole write path still works.
	body := `{"external_reference":"no-redis-` + uuid.NewString() + `","date_of_birth":"1990-01-01","sex":"UNKNOWN"}`
	created := h.do(http.MethodPost, "/api/v1/patients", body, h.token, nil)
	if created.Code != http.StatusCreated {
		t.Fatalf("create without Redis = %d: %s", created.Code, created.Body.String())
	}
	var patient model.Patient
	if err := json.Unmarshal(created.Body.Bytes(), &patient); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if rec := h.do(http.MethodGet, "/api/v1/patients/"+patient.ID.String(), "", h.token, nil); rec.Code != http.StatusOK {
		t.Errorf("read back without Redis = %d", rec.Code)
	}

	// The outage is reported once, not on every request.
	if outages := strings.Count(h.logs.String(), "redis unavailable"); outages > 1 {
		t.Errorf("the outage was logged %d times; it should be reported once", outages)
	}
}

// sameJSON compares two responses as documents. A replay is stored as jsonb
// and read back, so it is the same document but not always the same bytes.
func sameJSON(t *testing.T, a, b []byte) bool {
	t.Helper()
	var left, right any
	if err := json.Unmarshal(a, &left); err != nil {
		t.Fatalf("first response is not JSON: %v", err)
	}
	if err := json.Unmarshal(b, &right); err != nil {
		t.Fatalf("second response is not JSON: %v", err)
	}
	return reflect.DeepEqual(left, right)
}

// A gateway configured with no Redis at all is a supported deployment and
// says so at start-up.
func TestAGatewayWithNoRedisConfiguredStartsAndSaysSo(t *testing.T) {
	h := newRedisHarness(t, "", nil)

	if !strings.Contains(h.logs.String(), "REDIS_URL is not set") {
		t.Errorf("start-up did not report that Redis is absent: %s", h.logs.String())
	}
	if rec := h.do(http.MethodGet, "/ready", "", "", nil); rec.Code != http.StatusOK {
		t.Errorf("/ready = %d", rec.Code)
	}
	if rec := h.do(http.MethodGet, "/api/v1/patients/"+h.patientID.String(), "", h.token, nil); rec.Code != http.StatusOK {
		t.Errorf("read = %d, want 200", rec.Code)
	}
}
