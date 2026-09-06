package auth

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/n0ah-n0wa/VitalMesh/services/api-gateway/internal/config"
	"github.com/n0ah-n0wa/VitalMesh/services/api-gateway/internal/domain"
	"github.com/n0ah-n0wa/VitalMesh/services/api-gateway/internal/requestid"
)

const (
	alicePassword = "alice-password-2026"
	aliceEmail    = "alice@example.com"
)

// memoryUsers is an in-memory UserStore.
type memoryUsers struct {
	byEmail map[string]domain.User
	err     error // returned by every call when set
	updated map[uuid.UUID]string
}

func (m *memoryUsers) GetByEmail(_ context.Context, email string) (domain.User, error) {
	if m.err != nil {
		return domain.User{}, m.err
	}
	u, ok := m.byEmail[email]
	if !ok {
		return domain.User{}, domain.New(domain.KindNotFound, "NOT_FOUND", "no such user")
	}
	return u, nil
}

func (m *memoryUsers) GetByID(_ context.Context, id uuid.UUID) (domain.User, error) {
	if m.err != nil {
		return domain.User{}, m.err
	}
	for _, u := range m.byEmail {
		if u.ID == id {
			return u, nil
		}
	}
	return domain.User{}, domain.New(domain.KindNotFound, "NOT_FOUND", "no such user")
}

func (m *memoryUsers) UpdatePasswordHash(_ context.Context, id uuid.UUID, hash string) error {
	if m.updated == nil {
		m.updated = map[uuid.UUID]string{}
	}
	m.updated[id] = hash
	for email, u := range m.byEmail {
		if u.ID == id {
			u.PasswordHash = hash
			m.byEmail[email] = u
		}
	}
	return nil
}

type recordedLogin struct {
	userID    uuid.UUID
	requestID string
}

type memoryAudit struct {
	logins []recordedLogin
	err    error
}

func (a *memoryAudit) RecordLogin(_ context.Context, userID uuid.UUID, requestID string) error {
	if a.err != nil {
		return a.err
	}
	a.logins = append(a.logins, recordedLogin{userID, requestID})
	return nil
}

type fixture struct {
	service *Service
	users   *memoryUsers
	audit   *memoryAudit
	tokens  *Tokens
	clock   *clock
	logs    *bytes.Buffer
	alice   domain.User
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	hasher := NewHasher(fastParams)
	hash, err := hasher.Hash(context.Background(), alicePassword)
	if err != nil {
		t.Fatal(err)
	}
	alice := domain.User{ID: uuid.New(), Email: aliceEmail, PasswordHash: hash, Role: domain.RoleOperator, Status: domain.UserActive}
	users := &memoryUsers{byEmail: map[string]domain.User{aliceEmail: alice}}
	audit := &memoryAudit{}
	c := &clock{t: epoch}
	tokens := NewTokens(testJWT(), c.now)
	var logs bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&logs, nil))

	service, err := NewService(users, audit, hasher, tokens, logger)
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	return &fixture{service: service, users: users, audit: audit, tokens: tokens, clock: c, logs: &logs, alice: alice}
}

func requestCtx(id string) context.Context {
	return requestid.NewContext(context.Background(), id)
}

func TestLoginSucceeds(t *testing.T) {
	f := newFixture(t)
	session, err := f.service.Login(requestCtx("req-1"), aliceEmail, alicePassword)
	if err != nil {
		t.Fatalf("Login: %v", err)
	}
	if session.User.ID != f.alice.ID || session.Claims.Subject != f.alice.ID || session.Claims.Role != domain.RoleOperator {
		t.Errorf("session = %+v", session)
	}
	claims, err := f.tokens.Verify(session.Token)
	if err != nil || claims != session.Claims {
		t.Errorf("issued token does not verify: %v (claims %+v)", err, claims)
	}
	if len(f.audit.logins) != 1 || f.audit.logins[0] != (recordedLogin{f.alice.ID, "req-1"}) {
		t.Errorf("audit = %+v, want one LOGIN for alice under req-1", f.audit.logins)
	}
	if len(f.users.updated) != 0 {
		t.Error("hash was rewritten although its parameters are current")
	}
}

func TestLoginNormalisesEmail(t *testing.T) {
	f := newFixture(t)
	if _, err := f.service.Login(requestCtx("r"), "  Alice@Example.COM ", alicePassword); err != nil {
		t.Errorf("Login with differently-cased email: %v", err)
	}
}

func TestLoginRejectsInvalidCredentials(t *testing.T) {
	f := newFixture(t)
	cases := map[string][2]string{
		"wrong password":    {aliceEmail, "alice-password-2027"},
		"empty password":    {aliceEmail, ""},
		"unknown email":     {"nobody@example.com", alicePassword},
		"password of other": {"nobody@example.com", "whatever-it-is"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := f.service.Login(requestCtx("r"), c[0], c[1])
			var domErr *domain.Error
			if !errors.As(err, &domErr) || domErr.Kind != domain.KindUnauthorized || domErr.Code != CodeInvalidCredentials {
				t.Fatalf("err = %v, want %s", err, CodeInvalidCredentials)
			}
			if strings.Contains(domErr.Message, "password") && strings.Contains(domErr.Message, "wrong") {
				t.Errorf("message distinguishes wrong password from unknown account: %q", domErr.Message)
			}
		})
	}
	if len(f.audit.logins) != 0 {
		t.Errorf("failed logins were audited as LOGIN: %+v", f.audit.logins)
	}
}

func TestLoginFailuresTakeTheSameTime(t *testing.T) {
	if testing.Short() {
		t.Skip("timing comparison")
	}
	f := newFixture(t)
	measure := func(email string) time.Duration {
		start := time.Now()
		for range 5 {
			_, _ = f.service.Login(requestCtx("r"), email, "not-the-password")
		}
		return time.Since(start)
	}
	known := measure(aliceEmail)
	unknown := measure("nobody@example.com")
	// Both paths run one Argon2 verification; a missing verification would
	// make the unknown path orders of magnitude faster.
	if unknown < known/4 {
		t.Errorf("unknown email answered in %s vs %s for a known one: timing reveals account existence", unknown, known)
	}
}

func TestLoginRejectsDisabledAccountOnlyWithCorrectPassword(t *testing.T) {
	f := newFixture(t)
	disabled := f.alice
	disabled.Status = domain.UserDisabled
	f.users.byEmail[aliceEmail] = disabled

	_, err := f.service.Login(requestCtx("r"), aliceEmail, alicePassword)
	var domErr *domain.Error
	if !errors.As(err, &domErr) || domErr.Kind != domain.KindForbidden || domErr.Code != CodeAccountDisabled {
		t.Errorf("disabled with right password: err = %v, want %s", err, CodeAccountDisabled)
	}
	_, err = f.service.Login(requestCtx("r"), aliceEmail, "wrong")
	if !errors.As(err, &domErr) || domErr.Code != CodeInvalidCredentials {
		t.Errorf("disabled with wrong password: err = %v, want %s", err, CodeInvalidCredentials)
	}
}

func TestLoginUpgradesOutdatedHash(t *testing.T) {
	f := newFixture(t)
	weak := NewHasher(config.PasswordHash{MemoryKiB: 8 * 1024, Time: 2, Parallelism: 2})
	oldHash, _ := weak.Hash(context.Background(), alicePassword)
	alice := f.alice
	alice.PasswordHash = oldHash
	f.users.byEmail[aliceEmail] = alice

	if _, err := f.service.Login(requestCtx("r"), aliceEmail, alicePassword); err != nil {
		t.Fatalf("Login: %v", err)
	}
	upgraded, ok := f.users.updated[f.alice.ID]
	if !ok || !strings.HasPrefix(upgraded, "$argon2id$v=19$m=8192,t=1,p=1$") {
		t.Fatalf("hash not upgraded: %q", upgraded)
	}
	if _, err := f.service.Login(requestCtx("r"), aliceEmail, alicePassword); err != nil {
		t.Errorf("Login with upgraded hash: %v", err)
	}
}

func TestLoginFailsClosedWhenAuditFails(t *testing.T) {
	f := newFixture(t)
	f.audit.err = errors.New("audit_logs unavailable")
	_, err := f.service.Login(requestCtx("r"), aliceEmail, alicePassword)
	var domErr *domain.Error
	if err == nil || errors.As(err, &domErr) {
		t.Errorf("err = %v, want an internal (unclassified) error", err)
	}
}

func TestLoginReportsStoreAndHashFailuresAsInternal(t *testing.T) {
	f := newFixture(t)
	f.users.err = errors.New("connection refused")
	var domErr *domain.Error
	if _, err := f.service.Login(requestCtx("r"), aliceEmail, alicePassword); err == nil || errors.As(err, &domErr) {
		t.Errorf("store failure: err = %v, want internal", err)
	}

	f.users.err = nil
	corrupt := f.alice
	corrupt.PasswordHash = "not-a-hash"
	f.users.byEmail[aliceEmail] = corrupt
	if _, err := f.service.Login(requestCtx("r"), aliceEmail, alicePassword); !errors.Is(err, ErrInvalidHash) {
		t.Errorf("corrupt hash: err = %v, want ErrInvalidHash", err)
	}
}

func TestLoginNeverLogsCredentialsOrTokens(t *testing.T) {
	f := newFixture(t)
	session, err := f.service.Login(requestCtx("r"), aliceEmail, alicePassword)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = f.service.Login(requestCtx("r"), aliceEmail, "wrong-password-value")
	_, _ = f.service.Login(requestCtx("r"), "unknown-person@example.com", "another-secret-value")

	logs := f.logs.String()
	for _, secret := range []string{alicePassword, "wrong-password-value", "another-secret-value", session.Token, f.alice.PasswordHash, aliceEmail, "unknown-person"} {
		if strings.Contains(logs, secret) {
			t.Errorf("logs contain %q:\n%s", secret, logs)
		}
	}
	if !strings.Contains(logs, "login succeeded") || !strings.Contains(logs, `"reason":"wrong_password"`) || !strings.Contains(logs, `"reason":"unknown_email"`) {
		t.Errorf("expected outcome records missing:\n%s", logs)
	}
}

func TestCurrentUser(t *testing.T) {
	f := newFixture(t)
	principal := Principal{UserID: f.alice.ID, Role: domain.RoleOperator}
	user, err := f.service.CurrentUser(context.Background(), principal)
	if err != nil || user.ID != f.alice.ID {
		t.Fatalf("CurrentUser = %+v, %v", user, err)
	}

	var domErr *domain.Error
	_, err = f.service.CurrentUser(context.Background(), Principal{UserID: uuid.New()})
	if !errors.As(err, &domErr) || domErr.Code != CodeInvalidToken {
		t.Errorf("deleted subject: err = %v, want %s", err, CodeInvalidToken)
	}

	disabled := f.alice
	disabled.Status = domain.UserDisabled
	f.users.byEmail[aliceEmail] = disabled
	_, err = f.service.CurrentUser(context.Background(), principal)
	if !errors.As(err, &domErr) || domErr.Code != CodeAccountDisabled {
		t.Errorf("disabled subject: err = %v, want %s", err, CodeAccountDisabled)
	}
}

func TestPrincipalContext(t *testing.T) {
	if _, ok := FromContext(context.Background()); ok {
		t.Error("empty context yields a principal")
	}
	claims := Claims{Subject: uuid.New(), Role: domain.RoleAdmin, TokenID: "t", ExpiresAt: epoch}
	p, ok := FromContext(NewContext(context.Background(), PrincipalOf(claims)))
	if !ok || p.UserID != claims.Subject || p.Role != domain.RoleAdmin || p.TokenID != "t" || !p.ExpiresAt.Equal(epoch) {
		t.Errorf("principal = %+v, %v", p, ok)
	}
}

func TestTokenErrorMapping(t *testing.T) {
	if e := TokenError(ErrTokenExpired); e.Code != CodeTokenExpired || e.Kind != domain.KindUnauthorized {
		t.Errorf("expired -> %+v", e)
	}
	for _, err := range []error{ErrTokenMalformed, ErrTokenSignature, ErrTokenUnknownKey, ErrTokenClaims} {
		if e := TokenError(err); e.Code != CodeInvalidToken || e.Kind != domain.KindUnauthorized || !errors.Is(e, err) {
			t.Errorf("%v -> %+v", err, e)
		}
	}
}
