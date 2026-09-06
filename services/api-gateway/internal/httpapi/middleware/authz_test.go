package middleware

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/n0ah-n0wa/VitalMesh/services/api-gateway/internal/auth"
	"github.com/n0ah-n0wa/VitalMesh/services/api-gateway/internal/authz"
	"github.com/n0ah-n0wa/VitalMesh/services/api-gateway/internal/domain"
)

func requireHandler(t *testing.T, permission authz.Permission) (http.Handler, *int) {
	t.Helper()
	logger, _ := testLogger()
	calls := 0
	h := Chain(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.WriteHeader(http.StatusNoContent)
	}), RequestID(), Require(authz.Default(), permission, logger))
	return h, &calls
}

func asRole(role domain.Role) *http.Request {
	req := httptest.NewRequest(http.MethodDelete, "/api/v1/patients/x", nil)
	principal := auth.Principal{UserID: uuid.New(), Role: role, TokenID: "t"}
	return req.WithContext(auth.NewContext(req.Context(), principal))
}

func TestRequireAllowsGrantedRoles(t *testing.T) {
	h, calls := requireHandler(t, authz.PatientsWrite)
	for _, role := range []domain.Role{domain.RoleAdmin, domain.RoleOperator} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, asRole(role))
		if rec.Code != http.StatusNoContent {
			t.Errorf("%s: status = %d, want 204 (%s)", role, rec.Code, rec.Body.String())
		}
	}
	if *calls != 2 {
		t.Errorf("handler ran %d times, want 2", *calls)
	}
}

func TestRequireDeniesRolesWithoutThePermission(t *testing.T) {
	h, calls := requireHandler(t, authz.PatientsWrite)
	for _, role := range []domain.Role{domain.RoleUser, "ROOT", ""} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, asRole(role))
		if rec.Code != http.StatusForbidden {
			t.Errorf("%q: status = %d, want 403", role, rec.Code)
		}
		if code := decodeErrorCode(t, rec); code != authz.CodePermissionDenied {
			t.Errorf("%q: code = %q", role, code)
		}
		if rec.Header().Get("WWW-Authenticate") != "" {
			t.Errorf("%q: a 403 must not challenge for credentials", role)
		}
		if strings.Contains(rec.Body.String(), "patients:write") {
			t.Errorf("%q: response names the permission", role)
		}
	}
	if *calls != 0 {
		t.Errorf("handler ran %d times for denied requests", *calls)
	}
}

func TestRequireWithoutPrincipalFailsClosed(t *testing.T) {
	h, calls := requireHandler(t, authz.PatientsRead)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/patients", nil))
	if rec.Code != http.StatusUnauthorized || decodeErrorCode(t, rec) != auth.CodeAuthenticationRequired {
		t.Errorf("status = %d body = %s", rec.Code, rec.Body.String())
	}
	if rec.Header().Get("WWW-Authenticate") != `Bearer realm="vitalmesh"` {
		t.Errorf("WWW-Authenticate = %q", rec.Header().Get("WWW-Authenticate"))
	}
	if *calls != 0 {
		t.Error("handler ran without a principal")
	}
}

func TestRequireAuthenticatedAcceptsEveryRoleOnly(t *testing.T) {
	h, _ := requireHandler(t, authz.Authenticated)
	for _, role := range []domain.Role{domain.RoleAdmin, domain.RoleOperator, domain.RoleUser} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, asRole(role))
		if rec.Code != http.StatusNoContent {
			t.Errorf("%s: status = %d", role, rec.Code)
		}
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, asRole("GUEST"))
	if rec.Code != http.StatusForbidden {
		t.Errorf("unknown role: status = %d, want 403", rec.Code)
	}
}

func TestRequireLogsDenialsWithoutSecrets(t *testing.T) {
	logger, buf := testLogger()
	h := Chain(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}),
		RequestID(), Require(authz.Default(), authz.UsersManage, logger))
	req := httptest.NewRequest(http.MethodPost, "/api/v1/users", nil)
	req = req.WithContext(auth.NewContext(context.Background(), auth.Principal{UserID: uuid.New(), Role: domain.RoleOperator, TokenID: "secret-token-id"}))
	h.ServeHTTP(httptest.NewRecorder(), req)

	if !strings.Contains(buf.String(), "authorization denied") || !strings.Contains(buf.String(), `"role":"OPERATOR"`) {
		t.Errorf("denial not logged:\n%s", buf.String())
	}
	if strings.Contains(buf.String(), "secret-token-id") {
		t.Error("token id logged")
	}
}
