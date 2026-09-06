//go:build integration

package app

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/n0ah-n0wa/VitalMesh/services/api-gateway/internal/auth"
	"github.com/n0ah-n0wa/VitalMesh/services/api-gateway/internal/config"
	"github.com/n0ah-n0wa/VitalMesh/services/api-gateway/internal/domain"
	"github.com/n0ah-n0wa/VitalMesh/services/api-gateway/internal/httpapi/model"
	"github.com/n0ah-n0wa/VitalMesh/services/api-gateway/internal/infra/postgres"
	"github.com/n0ah-n0wa/VitalMesh/services/api-gateway/internal/infra/postgres/postgrestest"
)

// The login flow end to end: a real schema, the real repositories, the
// wired route table and middleware chain.
func TestLoginAgainstPostgres(t *testing.T) {
	pool, dbURL, _ := postgrestest.New(t)
	cfg, err := config.Load(func(key string) (string, bool) {
		switch key {
		case "DATABASE_URL":
			return dbURL, true
		case "JWT_SECRET":
			return "integration-secret-integration-secret", true
		case "JWT_TTL":
			return "2m", true
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
	a, err := New(context.Background(), cfg, slog.New(slog.NewJSONHandler(&logs, nil)), "v-int")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer a.pool.Close()

	const password = "correct-horse-battery"
	hash, err := auth.NewHasher(cfg.Auth.Password).Hash(context.Background(), password)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	alice, err := postgres.NewUsers(pool).Create(ctx, "alice@example.com", hash, domain.RoleAdmin)
	if err != nil {
		t.Fatalf("create user: %v", err)
	}

	post := func(body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/login", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Request-ID", "int-login-1")
		rec := httptest.NewRecorder()
		a.Handler().ServeHTTP(rec, req)
		return rec
	}
	me := func(authorization string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/auth/me", nil)
		if authorization != "" {
			req.Header.Set("Authorization", authorization)
		}
		rec := httptest.NewRecorder()
		a.Handler().ServeHTTP(rec, req)
		return rec
	}

	// Valid login.
	rec := post(`{"email":"ALICE@example.com","password":"` + password + `"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("login: status = %d (%s)", rec.Code, rec.Body.String())
	}
	var session model.LoginResponse
	if err := json.NewDecoder(rec.Body).Decode(&session); err != nil {
		t.Fatal(err)
	}
	if session.User.ID != alice.ID || session.User.Role != "ADMIN" || session.ExpiresIn != 120 {
		t.Errorf("session = %+v", session)
	}

	// The token opens the protected route.
	rec = me("Bearer " + session.AccessToken)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), alice.ID.String()) {
		t.Errorf("me: %d %s", rec.Code, rec.Body.String())
	}

	// Missing, tampered and expired tokens are refused by the wired chain.
	if rec := me(""); rec.Code != http.StatusUnauthorized {
		t.Errorf("me without token: status = %d", rec.Code)
	}
	if rec := me("Bearer " + session.AccessToken[:len(session.AccessToken)-3] + "abc"); rec.Code != http.StatusUnauthorized {
		t.Errorf("me with tampered token: status = %d", rec.Code)
	}
	past := auth.NewTokens(cfg.Auth.JWT, func() time.Time { return time.Now().Add(-time.Hour) })
	expired, _, _ := past.Issue(alice.ID, alice.Role)
	rec = me("Bearer " + expired)
	if rec.Code != http.StatusUnauthorized || !strings.Contains(rec.Body.String(), auth.CodeTokenExpired) {
		t.Errorf("me with expired token: %d %s", rec.Code, rec.Body.String())
	}

	// Wrong password.
	rec = post(`{"email":"alice@example.com","password":"wrong-horse-battery"}`)
	if rec.Code != http.StatusUnauthorized || !strings.Contains(rec.Body.String(), auth.CodeInvalidCredentials) {
		t.Errorf("wrong password: %d %s", rec.Code, rec.Body.String())
	}

	// Exactly one LOGIN audit entry, for the successful attempt, under the
	// request's id.
	entries, err := postgres.NewAudit(pool).ListByResource(ctx, alice.ID, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Action != domain.AuditLogin || entries[0].RequestID != "int-login-1" || entries[0].ActorID == nil || *entries[0].ActorID != alice.ID {
		t.Errorf("audit entries = %+v", entries)
	}

	// Nothing sensitive reached the logs.
	for _, secret := range []string{password, "wrong-horse-battery", session.AccessToken, hash} {
		if strings.Contains(logs.String(), secret) {
			t.Errorf("logs contain %q", secret)
		}
	}
}
