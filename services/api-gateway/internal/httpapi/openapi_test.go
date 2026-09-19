package httpapi

import (
	"encoding/json"
	"net/http"
	"os"
	"sort"
	"strings"
	"testing"

	"github.com/n0ah-n0wa/VitalMesh/services/api-gateway/internal/authz"
	"github.com/n0ah-n0wa/VitalMesh/services/api-gateway/internal/httpapi/handler"
)

// publicContractPath is the machine-readable description of the public API,
// relative to this package (SPECIFICATIONS.md section 27).
const publicContractPath = "../../../../contracts/openapi/vitalmesh-public-v1.json"

// The unversioned routes NewHandler always registers. /metrics is
// deliberately not in the contract: it is a Prometheus exposition rather
// than JSON, it serves the platform's scraper rather than clients, and
// deployments restrict it at the network.
var unversionedContractRoutes = []string{
	"GET /health",
	"GET /ready",
}

// The contract and the router must describe the same API. This is the check
// that keeps the published document honest: it derives the served route set
// from the same tables NewHandler builds from, so an endpoint that is added,
// removed or left unimplemented cannot silently disagree with what clients
// are told.
//
// It is also what makes the two unserved authorization rules visible. Users
// and job cancellation have rules in authz.Routes and no handler, so they
// are not mounted and must not appear in the contract; if a handler is added
// later, this test fails until the contract catches up.
func TestPublicContractDescribesExactlyTheMountedRoutes(t *testing.T) {
	mounted := mountedRoutes()
	documented := documentedRoutes(t)

	for _, route := range mounted {
		if !contains(documented, route) {
			t.Errorf("%s is served but is not in the public contract; a client has no "+
				"machine-readable description of an endpoint it can call", route)
		}
	}
	for _, route := range documented {
		if !contains(mounted, route) {
			t.Errorf("%s is in the public contract but is not served; the contract promises "+
				"an endpoint that answers 404", route)
		}
	}
	if len(mounted) != len(documented) {
		t.Errorf("mounted %d routes, contract describes %d", len(mounted), len(documented))
	}
	t.Logf("contract and router agree on %d routes", len(mounted))
}

// An authorization rule without a handler is a deliberate choice, not an
// oversight, and the contract must not describe it. This states the current
// set so that serving one becomes a visible decision.
func TestUnservedAuthorizationRulesAreAbsentFromTheContract(t *testing.T) {
	want := []string{
		"POST /api/v1/users",
		"GET /api/v1/users",
		"GET /api/v1/users/{user_id}",
		"PATCH /api/v1/users/{user_id}/role",
		"POST /api/v1/processing/jobs/{job_id}/cancel",
	}

	mounted := mountedRoutes()
	var unserved []string
	for _, rule := range authz.Routes {
		route := rule.Method + " " + APIv1 + rule.Path
		if !contains(mounted, route) {
			unserved = append(unserved, route)
		}
	}
	sort.Strings(unserved)
	sort.Strings(want)
	if strings.Join(unserved, ", ") != strings.Join(want, ", ") {
		t.Errorf("unserved authorization rules changed:\n  got  %v\n  want %v\n"+
			"Serving or removing one is a decision that must be reflected here and in "+
			"the public contract.", unserved, want)
	}

	documented := documentedRoutes(t)
	for _, route := range unserved {
		if contains(documented, route) {
			t.Errorf("%s has no handler but the contract describes it", route)
		}
	}
}

// mountedRoutes is every route NewHandler registers, given every feature
// handler. The handlers are zero values: operations only tests them for
// nil, so this yields the full set without constructing their dependencies.
func mountedRoutes() []string {
	h := Handlers{
		Health:       &handler.Health{},
		Auth:         &handler.Auth{},
		Patients:     &handler.Patients{},
		Measurements: &handler.Measurements{},
		Processing:   &handler.Processing{},
	}
	routes := append([]string{}, unversionedContractRoutes...)
	for key := range h.operations() {
		routes = append(routes, key.Method+" "+APIv1+key.Path)
	}
	sort.Strings(routes)
	return routes
}

// documentedRoutes is every operation the public contract declares, in the
// same "METHOD /path" spelling as the router.
func documentedRoutes(t *testing.T) []string {
	t.Helper()
	raw, err := os.ReadFile(publicContractPath)
	if err != nil {
		t.Fatalf("read public contract: %v", err)
	}
	var doc struct {
		Paths map[string]map[string]json.RawMessage `json:"paths"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("parse public contract: %v", err)
	}
	if len(doc.Paths) == 0 {
		t.Fatal("the public contract declares no paths")
	}

	methods := map[string]string{
		"get": http.MethodGet, "post": http.MethodPost, "put": http.MethodPut,
		"patch": http.MethodPatch, "delete": http.MethodDelete,
	}
	var routes []string
	for path, item := range doc.Paths {
		for method := range item {
			if upper, ok := methods[method]; ok {
				routes = append(routes, upper+" "+path)
			}
		}
	}
	sort.Strings(routes)
	return routes
}

func contains(list []string, want string) bool {
	for _, entry := range list {
		if entry == want {
			return true
		}
	}
	return false
}
