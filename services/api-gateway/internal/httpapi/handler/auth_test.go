package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
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
)

const (
	testEmail    = "alice@example.com"
	testPassword = "alice-password-2026"
)

type fakeUsers struct {
	user domain.User
	err  error
}

func (f *fakeUsers) GetByEmail(_ context.Context, email string) (domain.User, error) {
	if f.err != nil {
		return domain.User{}, f.err
	}
	if email != f.user.Email {
		return domain.User{}, domain.New(domain.KindNotFound, "NOT_FOUND", "no such user")
	}
	return f.user, nil
}

func (f *fakeUsers) GetByID(_ context.Context, id uuid.UUID) (domain.User, error) {
	if f.err != nil {
		return domain.User{}, f.err
	}
	if id != f.user.ID {
		return domain.User{}, domain.New(domain.KindNotFound, "NOT_FOUND", "no such user")
	}
	return f.user, nil
}

func (f *fakeUsers) UpdatePasswordHash(context.Context, uuid.UUID, string) error { return nil }

type fakeAudit struct{ logins int }

func (a *fakeAudit) RecordLogin(context.Context, uuid.UUID, string) error {
	a.logins++
	return nil
}

type authFixture struct {
	handler *Auth
	users   *fakeUsers
	audit   *fakeAudit
	tokens  *auth.Tokens
	logs    *bytes.Buffer
	now     time.Time
}

func newAuthFixture(t *testing.T) *authFixture {
	t.Helper()
	hasher := auth.NewHasher(config.PasswordHash{MemoryKiB: 8 * 1024, Time: 1, Parallelism: 1})
	hash, err := hasher.Hash(context.Background(), testPassword)
	if err != nil {
		t.Fatal(err)
	}
	f := &authFixture{
		users: &fakeUsers{user: domain.User{
			ID: uuid.New(), Email: testEmail, PasswordHash: hash, Role: domain.RoleOperator, Status: domain.UserActive,
			CreatedAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
		}},
		audit: &fakeAudit{},
		logs:  &bytes.Buffer{},
		now:   time.Date(2026, 9, 6, 12, 0, 0, 0, time.UTC),
	}
	f.tokens = auth.NewTokens(config.JWT{
		KeyID: "k1", Secret: []byte("handler-secret-handler-secret-32!!"), Issuer: "vitalmesh-test",
		TTL: 15 * time.Minute, ClockSkew: 30 * time.Second,
	}, func() time.Time { return f.now })
	logger := slog.New(slog.NewJSONHandler(f.logs, nil))
	service, err := auth.NewService(f.users, f.audit, hasher, f.tokens, logger)
	if err != nil {
		t.Fatal(err)
	}
	f.handler = NewAuth(service, logger)
	return f
}

func (f *authFixture) login(body string, contentType string) *httptest.ResponseRecorder {
	var rd io.Reader
	if body != "" {
		rd = strings.NewReader(body)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/login", rd)
	req.Header.Set("Content-Type", contentType)
	rec := httptest.NewRecorder()
	f.handler.Login(rec, req)
	return rec
}

func decodeError(t *testing.T, rec *httptest.ResponseRecorder) model.ErrorDetail {
	t.Helper()
	var body model.ErrorResponse
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatalf("decode envelope: %v (%s)", err, rec.Body.String())
	}
	return body.Error
}

func TestLoginValidCredentials(t *testing.T) {
	f := newAuthFixture(t)
	rec := f.login(`{"email":"Alice@Example.com","password":"`+testPassword+`"}`, "application/json")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (%s)", rec.Code, rec.Body.String())
	}
	var body model.LoginResponse
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if body.TokenType != "Bearer" || body.ExpiresIn != 900 || !body.ExpiresAt.Equal(f.now.Add(15*time.Minute)) {
		t.Errorf("token metadata = %+v", body)
	}
	if body.User.ID != f.users.user.ID || body.User.Email != testEmail || body.User.Role != "OPERATOR" || body.User.Status != "ACTIVE" {
		t.Errorf("user = %+v", body.User)
	}
	claims, err := f.tokens.Verify(body.AccessToken)
	if err != nil || claims.Subject != f.users.user.ID || claims.Role != domain.RoleOperator {
		t.Errorf("access token does not verify: %v, %+v", err, claims)
	}
	if f.audit.logins != 1 {
		t.Errorf("audit logins = %d, want 1", f.audit.logins)
	}
	if strings.Contains(rec.Body.String(), "password_hash") || strings.Contains(rec.Body.String(), "argon2") {
		t.Error("response exposes the password hash")
	}
}

func TestLoginInvalidCredentials(t *testing.T) {
	f := newAuthFixture(t)
	cases := map[string]string{
		"wrong password": `{"email":"alice@example.com","password":"alice-password-2027"}`,
		"unknown email":  `{"email":"mallory@example.com","password":"` + testPassword + `"}`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			rec := f.login(body, "application/json")
			if rec.Code != http.StatusUnauthorized {
				t.Fatalf("status = %d, want 401 (%s)", rec.Code, rec.Body.String())
			}
			if e := decodeError(t, rec); e.Code != auth.CodeInvalidCredentials || e.Message != "The email or password is incorrect." {
				t.Errorf("envelope = %+v", e)
			}
		})
	}
	if f.audit.logins != 0 {
		t.Errorf("failed logins were audited: %d", f.audit.logins)
	}
}

func TestLoginDisabledAccount(t *testing.T) {
	f := newAuthFixture(t)
	f.users.user.Status = domain.UserDisabled
	rec := f.login(`{"email":"alice@example.com","password":"`+testPassword+`"}`, "application/json")
	if rec.Code != http.StatusForbidden || decodeError(t, rec).Code != auth.CodeAccountDisabled {
		t.Errorf("status = %d body = %s", rec.Code, rec.Body.String())
	}
}

func TestLoginValidation(t *testing.T) {
	f := newAuthFixture(t)
	cases := map[string]struct {
		body   string
		status int
		code   string
		fields []string
	}{
		"empty body":        {"", http.StatusBadRequest, "EMPTY_BODY", nil},
		"malformed json":    {`{"email":`, http.StatusBadRequest, "INVALID_JSON", nil},
		"unknown field":     {`{"email":"a@b.c","password":"x","remember":true}`, http.StatusBadRequest, "INVALID_JSON", []string{"remember"}},
		"missing both":      {`{}`, http.StatusUnprocessableEntity, "VALIDATION_FAILED", []string{"email", "password"}},
		"blank email":       {`{"email":"  ","password":"x"}`, http.StatusUnprocessableEntity, "VALIDATION_FAILED", []string{"email"}},
		"email without at":  {`{"email":"alice.example.com","password":"x"}`, http.StatusUnprocessableEntity, "VALIDATION_FAILED", []string{"email"}},
		"email with spaces": {`{"email":"al ice@example.com","password":"x"}`, http.StatusUnprocessableEntity, "VALIDATION_FAILED", []string{"email"}},
		"email too long":    {`{"email":"` + strings.Repeat("a", 320) + `@example.com","password":"x"}`, http.StatusUnprocessableEntity, "VALIDATION_FAILED", []string{"email"}},
		"empty password":    {`{"email":"alice@example.com","password":""}`, http.StatusUnprocessableEntity, "VALIDATION_FAILED", []string{"password"}},
		"password too long": {`{"email":"alice@example.com","password":"` + strings.Repeat("p", auth.MaxPasswordLength+1) + `"}`, http.StatusUnprocessableEntity, "VALIDATION_FAILED", []string{"password"}},
		"wrong type":        {`{"email":"alice@example.com","password":123}`, http.StatusBadRequest, "INVALID_JSON", []string{"password"}},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			rec := f.login(tc.body, "application/json")
			if rec.Code != tc.status {
				t.Fatalf("status = %d, want %d (%s)", rec.Code, tc.status, rec.Body.String())
			}
			e := decodeError(t, rec)
			if e.Code != tc.code {
				t.Errorf("code = %q, want %q", e.Code, tc.code)
			}
			got := map[string]bool{}
			for _, d := range e.Details {
				got[d.Field] = true
			}
			for _, field := range tc.fields {
				if !got[field] {
					t.Errorf("details lack field %q: %+v", field, e.Details)
				}
			}
			if strings.Contains(rec.Body.String(), testPassword) {
				t.Error("response echoes the password")
			}
		})
	}

	rec := f.login(`{"email":"alice@example.com","password":"x"}`, "text/plain")
	if rec.Code != http.StatusUnsupportedMediaType {
		t.Errorf("text/plain: status = %d, want 415", rec.Code)
	}
	if f.audit.logins != 0 {
		t.Errorf("invalid requests were audited: %d", f.audit.logins)
	}
}

func TestLoginHidesInternalFailures(t *testing.T) {
	f := newAuthFixture(t)
	f.users.err = errors.New("pq: connection to 10.0.0.9:5432 refused")
	rec := f.login(`{"email":"alice@example.com","password":"`+testPassword+`"}`, "application/json")
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "10.0.0.9") {
		t.Error("response leaks the failure cause")
	}
	if !strings.Contains(f.logs.String(), "10.0.0.9") {
		t.Error("failure cause was not logged")
	}
	if strings.Contains(f.logs.String(), testPassword) {
		t.Error("password was logged")
	}
}

func TestMe(t *testing.T) {
	f := newAuthFixture(t)
	principal := auth.Principal{UserID: f.users.user.ID, Role: domain.RoleOperator, TokenID: "t1"}

	req := httptest.NewRequest(http.MethodGet, "/api/v1/auth/me", nil)
	rec := httptest.NewRecorder()
	f.handler.Me(rec, req.WithContext(auth.NewContext(req.Context(), principal)))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d (%s)", rec.Code, rec.Body.String())
	}
	var body model.User
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if body.ID != f.users.user.ID || body.Email != testEmail || body.Role != "OPERATOR" {
		t.Errorf("body = %+v", body)
	}

	rec = httptest.NewRecorder()
	f.handler.Me(rec, httptest.NewRequest(http.MethodGet, "/api/v1/auth/me", nil))
	if rec.Code != http.StatusUnauthorized || decodeError(t, rec).Code != auth.CodeAuthenticationRequired {
		t.Errorf("without principal: %d %s", rec.Code, rec.Body.String())
	}

	f.users.user.Status = domain.UserDisabled
	rec = httptest.NewRecorder()
	f.handler.Me(rec, req.WithContext(auth.NewContext(req.Context(), principal)))
	if rec.Code != http.StatusForbidden || decodeError(t, rec).Code != auth.CodeAccountDisabled {
		t.Errorf("disabled: %d %s", rec.Code, rec.Body.String())
	}
}
