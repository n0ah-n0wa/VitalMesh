// Package metrics is the instrumentation port of the application. Code
// records what happened through a Recorder; the observability phase binds a
// Prometheus exporter to it. Until then Noop keeps every call free.
//
// Labels are bounded by construction: routes are registered patterns, not
// paths; operations and outcomes are fixed vocabularies. Identifiers never
// become labels (SPECIFICATIONS.md section 41).
package metrics

import "time"

// Outcome classifies how an operation ended. The set is closed so that a
// metric label built from it stays bounded.
type Outcome string

// Outcomes of an application operation.
const (
	OutcomeOK        Outcome = "ok"
	OutcomeInvalid   Outcome = "invalid"
	OutcomeNotFound  Outcome = "not_found"
	OutcomeConflict  Outcome = "conflict"
	OutcomeDenied    Outcome = "denied"
	OutcomeError     Outcome = "error"
	OutcomeCancelled Outcome = "cancelled"
)

// Recorder receives measurements. Implementations must be safe for
// concurrent use and must never block the caller.
type Recorder interface {
	// HTTPRequest records one served request. route is the registered
	// pattern ("GET /api/v1/patients/{patient_id}") or "unmatched".
	HTTPRequest(method, route string, status int, duration time.Duration)
	// Operation records one application operation ("patient.create") and
	// its outcome.
	Operation(name string, outcome Outcome, duration time.Duration)
	// Batch records the size of one batch operation ("measurement.batch").
	Batch(name string, size int)
	// Cache records one cache lookup and whether it hit. The name is the
	// read it serves ("patient.get"), never a key, so labels stay bounded
	// and no identifier becomes one (SPECIFICATIONS.md section 41).
	Cache(name string, hit bool)
}

// Noop records nothing.
type Noop struct{}

func (Noop) HTTPRequest(string, string, int, time.Duration) {}
func (Noop) Operation(string, Outcome, time.Duration)       {}
func (Noop) Batch(string, int)                              {}
func (Noop) Cache(string, bool)                             {}

// OrNoop returns r, or Noop when r is nil, so callers never check.
func OrNoop(r Recorder) Recorder {
	if r == nil {
		return Noop{}
	}
	return r
}
