package authz

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/n0ah-n0wa/VitalMesh/services/api-gateway/internal/auth"
	"github.com/n0ah-n0wa/VitalMesh/services/api-gateway/internal/domain"
)

// expected is the permission matrix docs/API.md promises. The test below
// checks Default against it cell by cell, so a policy change must be made
// here (and in the documentation) deliberately.
var expected = map[Permission]map[domain.Role]bool{
	UsersManage:       {domain.RoleAdmin: true, domain.RoleOperator: false, domain.RoleUser: false},
	PatientsWrite:     {domain.RoleAdmin: true, domain.RoleOperator: true, domain.RoleUser: false},
	PatientsRead:      {domain.RoleAdmin: true, domain.RoleOperator: true, domain.RoleUser: true},
	MeasurementsWrite: {domain.RoleAdmin: true, domain.RoleOperator: true, domain.RoleUser: false},
	MeasurementsRead:  {domain.RoleAdmin: true, domain.RoleOperator: true, domain.RoleUser: true},
	JobsWrite:         {domain.RoleAdmin: true, domain.RoleOperator: true, domain.RoleUser: false},
	JobsRead:          {domain.RoleAdmin: true, domain.RoleOperator: true, domain.RoleUser: true},
	ResultsRead:       {domain.RoleAdmin: true, domain.RoleOperator: true, domain.RoleUser: true},
}

var roles = []domain.Role{domain.RoleAdmin, domain.RoleOperator, domain.RoleUser}

func TestDefaultPolicyMatchesDocumentedMatrix(t *testing.T) {
	policy := Default()
	if len(expected) != len(All) {
		t.Fatalf("expected matrix covers %d permissions, All has %d", len(expected), len(All))
	}
	for _, perm := range All {
		row, ok := expected[perm]
		if !ok {
			t.Fatalf("permission %s has no expectation", perm)
		}
		for _, role := range roles {
			if got := policy.Allows(role, perm); got != row[role] {
				t.Errorf("Allows(%s, %s) = %v, want %v", role, perm, got, row[role])
			}
		}
	}
}

func TestEveryRoleIsAuthenticatedAndPublicIsOpen(t *testing.T) {
	policy := Default()
	for _, role := range roles {
		if !policy.Allows(role, Authenticated) || !policy.Allows(role, Public) {
			t.Errorf("%s lacks the implicit permissions", role)
		}
	}
	if !policy.Allows(domain.Role(""), Public) || !policy.Allows(domain.Role("GUEST"), Public) {
		t.Error("Public must not depend on the role")
	}
}

func TestUnknownRolesHoldNothing(t *testing.T) {
	policy := Default()
	for _, role := range []domain.Role{"", "ROOT", "admin", "Admin", "ADMIN ", "SUPERUSER"} {
		if policy.Allows(role, Authenticated) {
			t.Errorf("unknown role %q counts as authenticated", role)
		}
		for _, perm := range All {
			if policy.Allows(role, perm) {
				t.Errorf("unknown role %q holds %s", role, perm)
			}
		}
	}
}

func TestPermissionsListsGrantsInOrder(t *testing.T) {
	policy := Default()
	user := policy.Permissions(domain.RoleUser)
	want := []Permission{PatientsRead, MeasurementsRead, JobsRead, ResultsRead}
	if !slices.Equal(user, want) {
		t.Errorf("USER permissions = %v, want %v", user, want)
	}
	if got := policy.Permissions(domain.RoleAdmin); !slices.Equal(got, All) {
		t.Errorf("ADMIN permissions = %v, want every permission", got)
	}
	if got := policy.Permissions("nobody"); got != nil {
		t.Errorf("unknown role permissions = %v, want none", got)
	}
}

func TestHierarchyIsStrict(t *testing.T) {
	// Each role's grants are a strict superset of the next role's, so
	// promoting an account never removes a capability.
	policy := Default()
	admin, operator, user := policy.Permissions(domain.RoleAdmin), policy.Permissions(domain.RoleOperator), policy.Permissions(domain.RoleUser)
	for _, p := range operator {
		if !slices.Contains(admin, p) {
			t.Errorf("OPERATOR holds %s but ADMIN does not", p)
		}
	}
	for _, p := range user {
		if !slices.Contains(operator, p) {
			t.Errorf("USER holds %s but OPERATOR does not", p)
		}
	}
	if len(admin) <= len(operator) || len(operator) <= len(user) {
		t.Errorf("hierarchy not strict: %d/%d/%d", len(admin), len(operator), len(user))
	}
}

func TestUnknownPermissionIsDeniedToEveryone(t *testing.T) {
	policy := Default()
	for _, role := range roles {
		if policy.Allows(role, Permission("system:*")) {
			t.Errorf("%s holds an undeclared permission", role)
		}
	}
}

func principalCtx(role domain.Role) context.Context {
	return auth.NewContext(context.Background(), auth.Principal{UserID: uuid.New(), Role: role, TokenID: "tok-1"})
}

func TestAuthorize(t *testing.T) {
	policy := Default()
	var logs bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&logs, nil))

	if err := policy.Authorize(principalCtx(domain.RoleUser), logger, PatientsRead); err != nil {
		t.Errorf("USER reading patients: %v", err)
	}
	if err := policy.Authorize(principalCtx(domain.RoleOperator), logger, Authenticated); err != nil {
		t.Errorf("OPERATOR authenticated: %v", err)
	}
	if err := policy.Authorize(context.Background(), logger, Public); err != nil {
		t.Errorf("anonymous public: %v", err)
	}

	var domErr *domain.Error
	err := policy.Authorize(principalCtx(domain.RoleUser), logger, UsersManage)
	if !errors.As(err, &domErr) || domErr.Kind != domain.KindForbidden || domErr.Code != CodePermissionDenied {
		t.Errorf("USER managing users: err = %v, want %s", err, CodePermissionDenied)
	}
	if strings.Contains(domErr.Message, "users:manage") || strings.Contains(domErr.Message, "USER") {
		t.Errorf("client message reveals policy internals: %q", domErr.Message)
	}
	if !strings.Contains(logs.String(), "authorization denied") || !strings.Contains(logs.String(), `"permission":"users:manage"`) || !strings.Contains(logs.String(), `"role":"USER"`) {
		t.Errorf("denial not logged with role and permission:\n%s", logs.String())
	}
	if strings.Contains(logs.String(), "tok-1") {
		t.Error("token id logged on denial")
	}

	for _, perm := range []Permission{Authenticated, PatientsRead, UsersManage} {
		err := policy.Authorize(context.Background(), logger, perm)
		if !errors.As(err, &domErr) || domErr.Kind != domain.KindUnauthorized || domErr.Code != auth.CodeAuthenticationRequired {
			t.Errorf("anonymous %s: err = %v, want %s", perm, err, auth.CodeAuthenticationRequired)
		}
	}
	err = policy.Authorize(principalCtx("ROOT"), logger, Authenticated)
	if !errors.As(err, &domErr) || domErr.Code != CodePermissionDenied {
		t.Errorf("unknown role: err = %v, want %s", err, CodePermissionDenied)
	}
}

// The route table must cover the specification's public operations and be
// internally consistent.
func TestRoutesCoverTheSpecification(t *testing.T) {
	required := []RouteKey{
		{http.MethodPost, "/patients"}, {http.MethodGet, "/patients"},
		{http.MethodGet, "/patients/{patient_id}"}, {http.MethodDelete, "/patients/{patient_id}"},
		{http.MethodPost, "/measurements"}, {http.MethodPost, "/measurements/batch"},
		{http.MethodGet, "/patients/{patient_id}/measurements"}, {http.MethodGet, "/measurements/{measurement_id}"},
		{http.MethodDelete, "/measurements/{measurement_id}"},
		{http.MethodPost, "/processing/jobs"}, {http.MethodGet, "/processing/jobs/{job_id}"},
		{http.MethodGet, "/patients/{patient_id}/processing-results"},
		{http.MethodPost, "/auth/login"}, {http.MethodGet, "/auth/me"},
	}
	for _, key := range required {
		if _, ok := Lookup(key.Method, key.Path); !ok {
			t.Errorf("no rule for %s %s", key.Method, key.Path)
		}
	}

	seen := map[RouteKey]bool{}
	for _, rule := range Routes {
		if seen[rule.Key()] {
			t.Errorf("duplicate rule for %s %s", rule.Method, rule.Path)
		}
		seen[rule.Key()] = true
		if !strings.HasPrefix(rule.Path, "/") || strings.HasSuffix(rule.Path, "/") || strings.HasPrefix(rule.Path, APIv1Guard) {
			t.Errorf("rule path %q must be relative to the version prefix", rule.Path)
		}
		switch rule.Method {
		case http.MethodGet, http.MethodPost, http.MethodPatch, http.MethodPut, http.MethodDelete:
		default:
			t.Errorf("rule %s %s has an unexpected method", rule.Method, rule.Path)
		}
		if rule.Permission != Public && rule.Permission != Authenticated && !slices.Contains(All, rule.Permission) {
			t.Errorf("rule %s %s requires undeclared permission %q", rule.Method, rule.Path, rule.Permission)
		}
	}
	if _, ok := Lookup(http.MethodGet, "/nope"); ok {
		t.Error("Lookup found an unlisted route")
	}
}

// APIv1Guard mirrors httpapi.APIv1 without importing the transport.
const APIv1Guard = "/api/"

func TestOnlyLoginIsPublicAndWritesNeedWriters(t *testing.T) {
	policy := Default()
	for _, rule := range Routes {
		switch {
		case rule.Permission == Public && rule.Key() != (RouteKey{http.MethodPost, "/auth/login"}):
			t.Errorf("%s %s is public", rule.Method, rule.Path)
		case rule.Method != http.MethodGet && rule.Permission != Public && policy.Allows(domain.RoleUser, rule.Permission) && rule.Permission != Authenticated:
			t.Errorf("USER may perform mutating operation %s %s", rule.Method, rule.Path)
		case strings.HasPrefix(rule.Path, "/users") && policy.Allows(domain.RoleOperator, rule.Permission):
			t.Errorf("OPERATOR may reach user management via %s %s", rule.Method, rule.Path)
		}
	}
}
