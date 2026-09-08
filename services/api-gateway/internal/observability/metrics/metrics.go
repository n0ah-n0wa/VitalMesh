// Package metrics is the instrumentation port of the application. Code
// records what happened through a Recorder; [Prometheus] implements it.
//
// # Bounded cardinality
//
// Every label this port can produce comes from a fixed vocabulary, and that
// is a property of the port rather than of its callers (SPECIFICATIONS.md
// section 41). Routes are the patterns the router registered, never request
// paths. Operation, outcome, statement verb, Redis command, rate-limit
// decision and backend are all closed sets. Nothing here accepts a value
// that varies per request, so a user id, patient id, request id or trace id
// cannot become a label even by mistake: there is no parameter to put one
// in.
package metrics

import (
	"strings"
	"time"
)

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
	// OutcomeUnavailable is a dependency that could not serve the call at
	// all, as distinct from one that answered with an error.
	OutcomeUnavailable Outcome = "unavailable"
)

// Statement verbs, the bounded label for database latency. Anything else a
// repository sends is recorded as [StatementOther] rather than widening the
// label space.
const (
	StatementSelect   = "select"
	StatementInsert   = "insert"
	StatementUpdate   = "update"
	StatementDelete   = "delete"
	StatementBegin    = "begin"
	StatementCommit   = "commit"
	StatementRollback = "rollback"
	StatementOther    = "other"
)

// MethodOther is the label for any request method outside the ones this
// API serves. The method is the one label whose raw value a client
// controls: HTTP permits any token as a method, so recording it verbatim
// would let a caller create a series per request. Only the methods the
// router can route are kept; everything else is one bucket.
const MethodOther = "OTHER"

// knownMethods are the methods any route in this application can be
// registered under.
var knownMethods = map[string]bool{
	"GET": true, "HEAD": true, "POST": true, "PUT": true,
	"PATCH": true, "DELETE": true, "OPTIONS": true,
}

// Method reduces a request method to a bounded label.
func Method(method string) string {
	if knownMethods[method] {
		return method
	}
	return MethodOther
}

// Rate-limit decisions, the bounded label for rate-limit events.
const (
	DecisionAllowed = "allowed"
	DecisionLimited = "limited"
)

// Rate-limit backends, the other bounded label for a decision.
const (
	BackendShared = "shared"
	BackendLocal  = "local"
)

// ProcessingJob names the gauge of jobs being processed right now. It lives
// here so that the exporter can publish it at zero before the first job
// runs and the service can move it, without either owning the name.
const ProcessingJob = "processing.job"

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
	// Database records one statement sent to PostgreSQL. verb is the
	// statement's leading keyword, normalised by [Statement], never the
	// statement itself: SQL carries values and would be unbounded.
	Database(verb string, outcome Outcome, duration time.Duration)
	// Redis records one command. command is the operation name ("get",
	// "eval"), never a key.
	Redis(command string, outcome Outcome, duration time.Duration)
	// RateLimit records one rate-limit decision. decision is
	// [DecisionAllowed] or [DecisionLimited]; backend says which counter
	// decided ("shared" or "local"). The caller is never a label: a budget
	// belongs to an account or an address, both of which are unbounded.
	RateLimit(decision, backend string)
	// InFlight moves the gauge of operations currently running by delta,
	// which is +1 when one starts and -1 when it ends. The name is a fixed
	// operation name ("processing.job").
	InFlight(name string, delta int)
}

// Statement normalises a SQL statement to the bounded verb used as a label.
// It reads only the leading keyword, so no value in the statement can reach
// a label.
func Statement(sql string) string {
	sql = strings.TrimLeft(sql, " \t\r\n")
	// Repository statements are written with the verb first; a leading
	// comment or CTE is reported as "other" rather than parsed.
	end := strings.IndexAny(sql, " \t\r\n(")
	if end < 0 {
		end = len(sql)
	}
	switch verb := strings.ToLower(sql[:end]); verb {
	case StatementSelect, StatementInsert, StatementUpdate, StatementDelete,
		StatementBegin, StatementCommit, StatementRollback:
		return verb
	default:
		return StatementOther
	}
}

// Noop records nothing.
type Noop struct{}

func (Noop) HTTPRequest(string, string, int, time.Duration) {}
func (Noop) Operation(string, Outcome, time.Duration)       {}
func (Noop) Batch(string, int)                              {}
func (Noop) Cache(string, bool)                             {}
func (Noop) Database(string, Outcome, time.Duration)        {}
func (Noop) Redis(string, Outcome, time.Duration)           {}
func (Noop) RateLimit(string, string)                       {}
func (Noop) InFlight(string, int)                           {}

// OrNoop returns r, or Noop when r is nil, so callers never check.
func OrNoop(r Recorder) Recorder {
	if r == nil {
		return Noop{}
	}
	return r
}
