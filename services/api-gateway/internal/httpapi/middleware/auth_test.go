package middleware

import (
	"encoding/json"
	"errors"
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

var authEpoch = time.Date(2026, 9, 6, 12, 0, 0, 0, time.UTC)

func authTokens(now *time.Time) *auth.Tokens {
	cfg := config.JWT{
		KeyID:     "k1",
		Secret:    []byte("middleware-secret-middleware-secret-32"),
		Issuer:    "vitalmesh-test",
		TTL:       15 * time.Minute,
		ClockSkew: 30 * time.Second,
	}
	return auth.NewTokens(cfg, func() time.Time { return *now })
}

// protected wraps a handler that reports the principal it was given.
func protected(t *testing.T, tokens *auth.Tokens) (http.Handler, *auth.Principal) {
	t.Helper()
	logger, _ := testLogger()
	var seen auth.Principal
	h := Chain(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p, ok := auth.FromContext(r.Context())
		if !ok {
			t.Error("handler ran without a principal")
		}
		seen = p
		w.WriteHeader(http.StatusNoContent)
	}), RequestID(), Authenticate(tokens, logger))
	return h, &seen
}

func get(h http.Handler, authorization string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, "/api/v1/auth/me", nil)
	if authorization != "" {
		req.Header.Set("Authorization", authorization)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestAuthenticateAcceptsValidBearerToken(t *testing.T) {
	now := authEpoch
	tokens := authTokens(&now)
	subject := uuid.New()
	token, claims, err := tokens.Issue(subject, domain.RoleAdmin)
	if err != nil {
		t.Fatal(err)
	}
	h, seen := protected(t, tokens)

	for _, header := range []string{"Bearer " + token, "bearer " + token, "Bearer  " + token + " "} {
		rec := get(h, header)
		if rec.Code != http.StatusNoContent {
			t.Errorf("%q: status = %d, want 204 (%s)", header, rec.Code, rec.Body.String())
			continue
		}
		if seen.UserID != subject || seen.Role != domain.RoleAdmin || seen.TokenID != claims.TokenID || !seen.ExpiresAt.Equal(claims.ExpiresAt) {
			t.Errorf("%q: principal = %+v", header, *seen)
		}
	}
}

func TestAuthenticateRejectsMissingToken(t *testing.T) {
	now := authEpoch
	h, _ := protected(t, authTokens(&now))

	rec := get(h, "")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
	if code := decodeErrorCode(t, rec); code != auth.CodeAuthenticationRequired {
		t.Errorf("code = %q, want %s", code, auth.CodeAuthenticationRequired)
	}
	if got := rec.Header().Get("WWW-Authenticate"); got != `Bearer realm="vitalmesh"` {
		t.Errorf("WWW-Authenticate = %q", got)
	}
}

func TestAuthenticateRejectsUnacceptableTokens(t *testing.T) {
	now := authEpoch
	tokens := authTokens(&now)
	valid, _, _ := tokens.Issue(uuid.New(), domain.RoleUser)
	parts := strings.Split(valid, ".")
	otherKey := auth.NewTokens(config.JWT{KeyID: "k1", Secret: []byte("someone-elses-secret-someone-elses-32"), Issuer: "vitalmesh-test", TTL: time.Hour, ClockSkew: time.Second}, func() time.Time { return now })
	forged, _, _ := otherKey.Issue(uuid.New(), domain.RoleAdmin)

	cases := map[string]string{
		"basic scheme":       "Basic " + valid,
		"no scheme":          valid,
		"empty bearer":       "Bearer ",
		"bearer only":        "Bearer",
		"garbage":            "Bearer not.a.token",
		"malformed":          "Bearer " + parts[0] + "." + parts[1],
		"tampered signature": "Bearer " + parts[0] + "." + parts[1] + "." + parts[2][:len(parts[2])-2] + "xx",
		"tampered payload":   "Bearer " + parts[0] + "." + parts[1][:len(parts[1])-2] + "xx." + parts[2],
		"forged key":         "Bearer " + forged,
	}
	h, _ := protected(t, tokens)
	for name, header := range cases {
		t.Run(name, func(t *testing.T) {
			rec := get(h, header)
			if rec.Code != http.StatusUnauthorized {
				t.Fatalf("status = %d, want 401 (%s)", rec.Code, rec.Body.String())
			}
			if code := decodeErrorCode(t, rec); code != auth.CodeInvalidToken {
				t.Errorf("code = %q, want %s", code, auth.CodeInvalidToken)
			}
			if got := rec.Header().Get("WWW-Authenticate"); !strings.Contains(got, `error="invalid_token"`) {
				t.Errorf("WWW-Authenticate = %q", got)
			}
		})
	}
}

func TestAuthenticateRejectsExpiredToken(t *testing.T) {
	now := authEpoch
	tokens := authTokens(&now)
	token, _, _ := tokens.Issue(uuid.New(), domain.RoleUser)
	h, _ := protected(t, tokens)

	now = authEpoch.Add(16 * time.Minute)
	rec := get(h, "Bearer "+token)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
	if code := decodeErrorCode(t, rec); code != auth.CodeTokenExpired {
		t.Errorf("code = %q, want %s", code, auth.CodeTokenExpired)
	}
}

func TestAuthenticateNeverLogsTheToken(t *testing.T) {
	now := authEpoch
	tokens := authTokens(&now)
	token, _, _ := tokens.Issue(uuid.New(), domain.RoleUser)
	logger, buf := testLogger()
	h := Chain(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}), RequestID(), Logging(logger), Authenticate(tokens, logger))

	now = authEpoch.Add(time.Hour)
	get(h, "Bearer "+token)
	get(h, "Bearer "+token+"tampered")
	get(h, "Bearer "+token[:len(token)-1])

	if strings.Contains(buf.String(), token[len(token)-20:]) || strings.Contains(buf.String(), token[:40]) {
		t.Errorf("token material logged:\n%s", buf.String())
	}
	if !strings.Contains(buf.String(), "authentication failed") || !strings.Contains(buf.String(), "expired") {
		t.Errorf("failure reason not logged:\n%s", buf.String())
	}
}

func TestAuthenticateErrorEnvelope(t *testing.T) {
	now := authEpoch
	h, _ := protected(t, authTokens(&now))
	rec := get(h, "Bearer x")

	var body model.ErrorResponse
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if body.Error.RequestID == "" || body.Error.Message == "" || strings.Contains(body.Error.Message, "malformed") {
		t.Errorf("envelope = %+v", body.Error)
	}
}

// failingVerifier proves the middleware maps any verifier error, not only
// the ones auth.Tokens produces today.
type failingVerifier struct{ err error }

func (f failingVerifier) Verify(string) (auth.Claims, error) { return auth.Claims{}, f.err }

func TestAuthenticateMapsVerifierErrors(t *testing.T) {
	logger, _ := testLogger()
	for _, tc := range []struct {
		err  error
		code string
	}{
		{auth.ErrTokenExpired, auth.CodeTokenExpired},
		{errors.New("something new"), auth.CodeInvalidToken},
	} {
		h := Chain(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
			t.Error("handler ran")
		}), Authenticate(failingVerifier{tc.err}, logger))
		rec := get(h, "Bearer whatever")
		if rec.Code != http.StatusUnauthorized || decodeErrorCode(t, rec) != tc.code {
			t.Errorf("%v: %d %s", tc.err, rec.Code, rec.Body.String())
		}
	}
}
