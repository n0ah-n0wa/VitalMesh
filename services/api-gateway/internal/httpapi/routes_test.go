package httpapi

import (
	"bytes"
	"encoding/base64"
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
	"github.com/n0ah-n0wa/VitalMesh/services/api-gateway/internal/httpapi/middleware"
	"github.com/n0ah-n0wa/VitalMesh/services/api-gateway/internal/observability/logging"
)

var (
	routesEpoch  = time.Date(2026, 9, 6, 12, 0, 0, 0, time.UTC)
	routesSecret = []byte("routes-secret-routes-secret-32!!!")
)

func routesJWT(secret []byte) config.JWT {
	return config.JWT{KeyID: "k1", Secret: secret, Issuer: "vitalmesh-test", TTL: 15 * time.Minute, ClockSkew: 30 * time.Second}
}

// mountedAPI mounts a stub handler for every rule in authz.Routes behind the
// real authentication middleware and the default policy. Each stub answers
// 200 with the rule it serves, so a response proves which guard ran.
func mountedAPI(t *testing.T) (http.Handler, *auth.Tokens, *bytes.Buffer) {
	t.Helper()
	var logBuf bytes.Buffer
	logger := logging.New(&logBuf, config.Log{Format: config.LogFormatJSON}, logging.Service{Name: "test"})
	tokens := auth.NewTokens(routesJWT(routesSecret), func() time.Time { return routesEpoch })

	ops := make(map[authz.RouteKey]http.Handler, len(authz.Routes))
	for _, rule := range authz.Routes {
		key := rule.Key()
		ops[key] = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			fmt.Fprintf(w, "%s %s", key.Method, key.Path)
		})
	}
	rt := NewRouter(logger)
	if err := Mount(rt.Group(APIv1), ops, middleware.Authenticate(tokens, logger), authz.Default(), nil, nil, logger); err != nil {
		t.Fatalf("Mount: %v", err)
	}
	return Wrap(config.HTTP{MaxBodyBytes: 1 << 20, RequestTimeout: time.Second}, logger, nil, rt), tokens, &logBuf
}

// concretePath replaces wildcards so the request matches the route.
func concretePath(path string) string {
	out := path
	for _, wildcard := range []string{"{user_id}", "{patient_id}", "{measurement_id}", "{job_id}"} {
		out = strings.ReplaceAll(out, wildcard, uuid.NewString())
	}
	return APIv1 + out
}

func send(h http.Handler, rule authz.Rule, authorization string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(rule.Method, concretePath(rule.Path), nil)
	if authorization != "" {
		req.Header.Set("Authorization", authorization)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func envelopeCode(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	var body struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatalf("decode envelope: %v (%s)", err, rec.Body.String())
	}
	return body.Error.Code
}

// Every role against every operation: the outcome must follow the policy
// exactly, and a served request must reach the handler of that operation.
func TestAuthorizationMatrix(t *testing.T) {
	h, tokens, _ := mountedAPI(t)
	policy := authz.Default()

	for _, role := range []domain.Role{domain.RoleAdmin, domain.RoleOperator, domain.RoleUser} {
		token, _, err := tokens.Issue(uuid.New(), role)
		if err != nil {
			t.Fatal(err)
		}
		for _, rule := range authz.Routes {
			t.Run(fmt.Sprintf("%s %s %s", role, rule.Method, rule.Path), func(t *testing.T) {
				rec := send(h, rule, "Bearer "+token)
				if policy.Allows(role, rule.Permission) {
					if rec.Code != http.StatusOK || rec.Body.String() != rule.Method+" "+rule.Path {
						t.Fatalf("allowed operation: %d %q", rec.Code, rec.Body.String())
					}
					return
				}
				if rec.Code != http.StatusForbidden || envelopeCode(t, rec) != authz.CodePermissionDenied {
					t.Fatalf("denied operation: %d %s", rec.Code, rec.Body.String())
				}
			})
		}
	}
}

func TestAllowedAndDeniedOperationsPerRole(t *testing.T) {
	// The matrix above is exhaustive; this spells out the intent per role
	// in the terms of docs/API.md so a regression reads as a sentence.
	h, tokens, _ := mountedAPI(t)
	issue := func(role domain.Role) string {
		token, _, err := tokens.Issue(uuid.New(), role)
		if err != nil {
			t.Fatal(err)
		}
		return "Bearer " + token
	}
	admin, operator, user := issue(domain.RoleAdmin), issue(domain.RoleOperator), issue(domain.RoleUser)

	cases := []struct {
		name   string
		auth   string
		method string
		path   string
		status int
	}{
		{"ADMIN creates users", admin, http.MethodPost, "/users", http.StatusOK},
		{"ADMIN changes roles", admin, http.MethodPatch, "/users/{user_id}/role", http.StatusOK},
		{"ADMIN deletes patients", admin, http.MethodDelete, "/patients/{patient_id}", http.StatusOK},
		{"ADMIN reads results", admin, http.MethodGet, "/patients/{patient_id}/processing-results", http.StatusOK},
		{"OPERATOR creates patients", operator, http.MethodPost, "/patients", http.StatusOK},
		{"OPERATOR submits measurements", operator, http.MethodPost, "/measurements/batch", http.StatusOK},
		{"OPERATOR cancels jobs", operator, http.MethodPost, "/processing/jobs/{job_id}/cancel", http.StatusOK},
		{"OPERATOR cannot list users", operator, http.MethodGet, "/users", http.StatusForbidden},
		{"OPERATOR cannot change roles", operator, http.MethodPatch, "/users/{user_id}/role", http.StatusForbidden},
		{"USER reads patients", user, http.MethodGet, "/patients", http.StatusOK},
		{"USER reads a measurement", user, http.MethodGet, "/measurements/{measurement_id}", http.StatusOK},
		{"USER reads a job", user, http.MethodGet, "/processing/jobs/{job_id}", http.StatusOK},
		{"USER sees own account", user, http.MethodGet, "/auth/me", http.StatusOK},
		{"USER cannot create patients", user, http.MethodPost, "/patients", http.StatusForbidden},
		{"USER cannot delete measurements", user, http.MethodDelete, "/measurements/{measurement_id}", http.StatusForbidden},
		{"USER cannot create jobs", user, http.MethodPost, "/processing/jobs", http.StatusForbidden},
		{"USER cannot create users", user, http.MethodPost, "/users", http.StatusForbidden},
		{"anyone may log in", "", http.MethodPost, "/auth/login", http.StatusOK},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rule, ok := authz.Lookup(tc.method, tc.path)
			if !ok {
				t.Fatalf("no rule for %s %s", tc.method, tc.path)
			}
			rec := send(h, rule, tc.auth)
			if rec.Code != tc.status {
				t.Errorf("status = %d, want %d (%s)", rec.Code, tc.status, rec.Body.String())
			}
		})
	}
}

func TestMissingAuthenticationOnEveryProtectedOperation(t *testing.T) {
	h, _, _ := mountedAPI(t)
	for _, rule := range authz.Routes {
		rec := send(h, rule, "")
		if rule.Permission == authz.Public {
			if rec.Code != http.StatusOK {
				t.Errorf("%s %s: public operation answered %d", rule.Method, rule.Path, rec.Code)
			}
			continue
		}
		if rec.Code != http.StatusUnauthorized || envelopeCode(t, rec) != auth.CodeAuthenticationRequired {
			t.Errorf("%s %s without token: %d %s", rule.Method, rule.Path, rec.Code, rec.Body.String())
		}
		if rec.Header().Get("WWW-Authenticate") == "" {
			t.Errorf("%s %s: no WWW-Authenticate challenge", rule.Method, rule.Path)
		}
	}
}

func TestPrivilegeEscalationAttempts(t *testing.T) {
	h, tokens, _ := mountedAPI(t)
	userID := uuid.New()
	userToken, _, err := tokens.Issue(userID, domain.RoleUser)
	if err != nil {
		t.Fatal(err)
	}
	parts := strings.Split(userToken, ".")
	rule, _ := authz.Lookup(http.MethodPost, "/users")

	// A USER token whose role claim is rewritten to ADMIN.
	rawClaims, _ := base64.RawURLEncoding.DecodeString(parts[1])
	var claims map[string]any
	if err := json.Unmarshal(rawClaims, &claims); err != nil {
		t.Fatal(err)
	}
	claims["role"] = "ADMIN"
	forgedClaims, _ := json.Marshal(claims)
	escalated := parts[0] + "." + base64.RawURLEncoding.EncodeToString(forgedClaims) + "." + parts[2]

	// An ADMIN token minted with a key the server does not know.
	rogue := auth.NewTokens(routesJWT([]byte("attacker-secret-attacker-secret-32")), func() time.Time { return routesEpoch })
	rogueAdmin, _, _ := rogue.Issue(userID, domain.RoleAdmin)

	// An ADMIN token signed with alg=none.
	noneHeader := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"none","kid":"k1"}`))
	unsigned := noneHeader + "." + base64.RawURLEncoding.EncodeToString(forgedClaims) + "."

	// A role the policy has never heard of, correctly signed (simulates a
	// stale or hand-edited token store).
	claims["role"] = "SUPERUSER"
	superClaims, _ := json.Marshal(claims)
	superuser := parts[0] + "." + base64.RawURLEncoding.EncodeToString(superClaims) + "." + parts[2]

	cases := map[string]struct {
		auth   string
		status int
		code   string
	}{
		"role claim rewritten":   {"Bearer " + escalated, http.StatusUnauthorized, auth.CodeInvalidToken},
		"admin token rogue key":  {"Bearer " + rogueAdmin, http.StatusUnauthorized, auth.CodeInvalidToken},
		"alg none":               {"Bearer " + unsigned, http.StatusUnauthorized, auth.CodeInvalidToken},
		"unknown role signed":    {"Bearer " + superuser, http.StatusUnauthorized, auth.CodeInvalidToken},
		"genuine user token":     {"Bearer " + userToken, http.StatusForbidden, authz.CodePermissionDenied},
		"role header spoofing":   {"Bearer " + userToken, http.StatusForbidden, authz.CodePermissionDenied},
		"no credentials at all":  {"", http.StatusUnauthorized, auth.CodeAuthenticationRequired},
		"basic auth as an admin": {"Basic YWRtaW46YWRtaW4=", http.StatusUnauthorized, auth.CodeInvalidToken},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			req := httptest.NewRequest(rule.Method, concretePath(rule.Path), nil)
			if tc.auth != "" {
				req.Header.Set("Authorization", tc.auth)
			}
			// Headers a proxy or client might try to assert a role with must
			// have no effect: only the verified token decides.
			req.Header.Set("X-Role", "ADMIN")
			req.Header.Set("X-User-Role", "ADMIN")
			req.Header.Set("X-Forwarded-User", "admin@example.com")
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			if rec.Code != tc.status || envelopeCode(t, rec) != tc.code {
				t.Errorf("status = %d, body = %s; want %d %s", rec.Code, rec.Body.String(), tc.status, tc.code)
			}
		})
	}
}

func TestExpiredTokenIsDeniedRegardlessOfRole(t *testing.T) {
	h, _, _ := mountedAPI(t)
	past := auth.NewTokens(routesJWT(routesSecret), func() time.Time { return routesEpoch.Add(-time.Hour) })
	expiredAdmin, _, _ := past.Issue(uuid.New(), domain.RoleAdmin)
	rule, _ := authz.Lookup(http.MethodGet, "/patients")
	rec := send(h, rule, "Bearer "+expiredAdmin)
	if rec.Code != http.StatusUnauthorized || envelopeCode(t, rec) != auth.CodeTokenExpired {
		t.Errorf("expired admin: %d %s", rec.Code, rec.Body.String())
	}
}

func TestMountRejectsUnlistedAndUnguardableOperations(t *testing.T) {
	logger := logging.New(&bytes.Buffer{}, config.Log{Format: config.LogFormatJSON}, logging.Service{})
	tokens := auth.NewTokens(routesJWT(routesSecret), nil)
	ok := http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})

	err := Mount(NewRouter(logger).Group(APIv1), map[authz.RouteKey]http.Handler{
		{Method: http.MethodDelete, Path: "/users/{user_id}"}: ok,
	}, middleware.Authenticate(tokens, logger), authz.Default(), nil, nil, logger)
	if err == nil || !strings.Contains(err.Error(), "no authorization rule") {
		t.Errorf("handler without rule: err = %v", err)
	}

	err = Mount(NewRouter(logger).Group(APIv1), map[authz.RouteKey]http.Handler{
		{Method: http.MethodGet, Path: "/auth/me"}: ok,
	}, nil, authz.Default(), nil, nil, logger)
	if err == nil || !strings.Contains(err.Error(), "needs authentication") {
		t.Errorf("protected route without authenticate: err = %v", err)
	}

	err = Mount(NewRouter(logger).Group(APIv1), nil, middleware.Authenticate(tokens, logger), nil, nil, nil, logger)
	if err == nil || !strings.Contains(err.Error(), "no authorization policy") {
		t.Errorf("nil policy: err = %v", err)
	}

	// Public operations mount without authentication.
	err = Mount(NewRouter(logger).Group(APIv1), map[authz.RouteKey]http.Handler{
		{Method: http.MethodPost, Path: "/auth/login"}: ok,
	}, nil, authz.Default(), nil, nil, logger)
	if err != nil {
		t.Errorf("public route without authenticate: %v", err)
	}
}

func TestUnimplementedOperationsAreNotServed(t *testing.T) {
	// Only operations with handlers are mounted; the rest stay 404 so a
	// rule never advertises an endpoint that does not exist yet.
	logger := logging.New(&bytes.Buffer{}, config.Log{Format: config.LogFormatJSON}, logging.Service{})
	tokens := auth.NewTokens(routesJWT(routesSecret), nil)
	rt := NewRouter(logger)
	ok := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	if err := Mount(rt.Group(APIv1), map[authz.RouteKey]http.Handler{{Method: http.MethodPost, Path: "/auth/login"}: ok}, middleware.Authenticate(tokens, logger), authz.Default(), nil, nil, logger); err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	rt.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/patients", nil))
	if rec.Code != http.StatusNotFound {
		t.Errorf("unimplemented operation answered %d, want 404", rec.Code)
	}
}
