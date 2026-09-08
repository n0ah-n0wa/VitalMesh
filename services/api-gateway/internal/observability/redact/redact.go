// Package redact is the single definition of what must never leave the
// process in telemetry (SPECIFICATIONS.md section 41).
//
// It is its own package rather than part of logging because logs, spans and
// metric labels are three exits from the process, and a rule that lives in
// one of them protects only that one. Logging applies these rules to every
// attribute; tracing applies them to every span attribute and every
// recorded error; metrics never accept a free-form value at all. One
// definition, three doors.
//
// Identifiers are deliberately kept. A user id, patient id, job id or
// request id is what makes telemetry useful during an incident, and each is
// an opaque handle rather than a fact about a person.
package redact

import (
	"regexp"
	"strconv"
	"strings"
)

// Redacted replaces a value that must never be recorded.
const Redacted = "[redacted]"

// MaxValueLength bounds any single recorded string. It is generous for an
// error message or an identifier and far too small for a request body, so
// a payload recorded by mistake is truncated rather than stored whole.
const MaxValueLength = 512

// sensitiveNames are attribute names whose value is a credential or a fact
// about a person. The match is exact, on the lower-cased name, so that
// deliberately useful neighbours survive: "token_id" is a token's public
// identifier and stays, while "token" is the credential itself and goes.
var sensitiveNames = map[string]bool{
	"authorization":      true,
	"cookie":             true,
	"credential":         true,
	"credentials":        true,
	"date_of_birth":      true,
	"dob":                true,
	"email":              true,
	"email_address":      true,
	"jwt":                true,
	"pass":               true,
	"passwd":             true,
	"password":           true,
	"password_hash":      true,
	"phone":              true,
	"secret":             true,
	"session":            true,
	"set_cookie":         true,
	"signature":          true,
	"ssn":                true,
	"token":              true,
	"national_id":        true,
	"personal_id":        true,
	"payload":            true,
	"body":               true,
	"request_body":       true,
	"response_body":      true,
	"external_reference": true,
}

// Counts are not payloads. Names such as "count", "readings" and
// "accepted" carry sizes rather than contents and are deliberately absent
// from the list above, because a size is what makes an incident legible.

// sensitiveSuffixes catch the same things under a qualified name, such as
// "internal_token" or "refresh_token". They are chosen not to match a
// useful identifier: nothing here matches "token_id" or "key".
var sensitiveSuffixes = []string{
	"_password",
	"_passwd",
	"_secret",
	"_token",
	"_jwt",
	"_credential",
	"_credentials",
	"_hash",
	"_email",
	"_signature",
}

// jwtPattern matches a JSON Web Token wherever it appears in a string,
// including inside an "Authorization: Bearer …" header echoed into an error
// message. It anchors on "eyJ", the base64url encoding of a JSON object's
// first two characters, which every JWT header begins with. Anchoring makes
// it precise: a hostname or a version string cannot match it.
var jwtPattern = regexp.MustCompile(`eyJ[A-Za-z0-9_-]{4,}\.[A-Za-z0-9_-]{4,}\.[A-Za-z0-9_-]*`)

// Sensitive reports whether a name must never carry its value into
// telemetry. The name may be a log attribute, a span attribute or anything
// else keyed by a name.
func Sensitive(name string) bool {
	lower := strings.ToLower(strings.TrimSpace(name))
	if known, ok := sensitiveNames[lower]; ok {
		return known
	}
	for _, suffix := range sensitiveSuffixes {
		if strings.HasSuffix(lower, suffix) {
			return true
		}
	}
	return false
}

// Scrub cleans one recorded string: a token found anywhere in it is
// removed, and what is left is bounded. It is the rule for a value whose
// name is not itself sensitive, such as an error message or a message.
func Scrub(s string) string {
	if strings.Contains(s, "eyJ") {
		s = jwtPattern.ReplaceAllString(s, Redacted)
	}
	return truncate(s)
}

// truncate bounds a string and says how much it dropped, so a reader can
// tell a short value from a shortened one.
func truncate(s string) string {
	if len(s) <= MaxValueLength {
		return s
	}
	dropped := len(s) - MaxValueLength
	return s[:MaxValueLength] + "…[" + strconv.Itoa(dropped) + " more bytes omitted]"
}
