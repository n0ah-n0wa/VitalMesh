package authz

import "net/http"

// Rule binds one API operation to the permission it requires. Path is
// relative to the API version prefix and uses net/http wildcards.
type Rule struct {
	Method     string
	Path       string
	Permission Permission
}

// Key identifies the operation the rule governs.
func (r Rule) Key() RouteKey { return RouteKey{Method: r.Method, Path: r.Path} }

// RouteKey identifies an operation by method and path.
type RouteKey struct {
	Method string
	Path   string
}

// Routes is the authorization table of the public API: every operation of
// SPECIFICATIONS.md sections 10–13 plus the authentication and user
// management operations, each with the permission it requires. The HTTP
// route table is built from it, so an operation cannot be served without an
// entry here, and this table is the one place to read or change who may do
// what.
var Routes = []Rule{
	// Authentication (docs/API.md "Authentication").
	{http.MethodPost, "/auth/login", Public},
	{http.MethodGet, "/auth/me", Authenticated},

	// Users (OPEN_QUESTIONS OQ-04).
	{http.MethodPost, "/users", UsersManage},
	{http.MethodGet, "/users", UsersManage},
	{http.MethodGet, "/users/{user_id}", UsersManage},
	{http.MethodPatch, "/users/{user_id}/role", UsersManage},

	// Patients (section 11).
	{http.MethodPost, "/patients", PatientsWrite},
	{http.MethodGet, "/patients", PatientsRead},
	{http.MethodGet, "/patients/{patient_id}", PatientsRead},
	{http.MethodDelete, "/patients/{patient_id}", PatientsWrite},

	// Measurements (section 12).
	{http.MethodPost, "/measurements", MeasurementsWrite},
	{http.MethodPost, "/measurements/batch", MeasurementsWrite},
	{http.MethodGet, "/measurements/{measurement_id}", MeasurementsRead},
	{http.MethodDelete, "/measurements/{measurement_id}", MeasurementsWrite},
	{http.MethodGet, "/patients/{patient_id}/measurements", MeasurementsRead},

	// Processing (section 13, OQ-06 for cancellation).
	{http.MethodPost, "/processing/jobs", JobsWrite},
	{http.MethodGet, "/processing/jobs/{job_id}", JobsRead},
	{http.MethodPost, "/processing/jobs/{job_id}/cancel", JobsWrite},
	{http.MethodGet, "/patients/{patient_id}/processing-results", ResultsRead},
}

// Lookup returns the rule for an operation.
func Lookup(method, path string) (Rule, bool) {
	for _, r := range Routes {
		if r.Method == method && r.Path == path {
			return r, true
		}
	}
	return Rule{}, false
}
