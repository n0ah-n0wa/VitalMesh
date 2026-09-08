//! Removal of anything that must never reach a log record
//! (SPECIFICATIONS.md section 41).
//!
//! Call sites in this service log counts and identifiers rather than
//! values, and reviewing them is how that stays true. This module exists
//! because review is a habit, and a habit holds only until someone adds a
//! field in a hurry. Every record passes through [`record`] on its way out,
//! so a credential logged by mistake is dropped by the formatter instead of
//! being written to disk and shipped to a log store, where it would have to
//! be handled as a disclosure.
//!
//! Identifiers are deliberately kept. A job id or a request id is what
//! makes a log useful during an incident and says nothing about a person.

use serde_json::{Map, Value};

/// Replaces a value that must never be logged.
pub(crate) const REDACTED: &str = "[redacted]";

/// Bounds any single logged string. Generous for a message or an
/// identifier, far too small for a batch of readings, so a payload logged
/// by mistake is truncated rather than stored whole.
pub(crate) const MAX_VALUE_LENGTH: usize = 512;

/// Field names whose value is a credential or a fact about a person. The
/// match is exact on the lower-cased name, so useful neighbours survive:
/// `token_id` identifies a token and stays, `token` is the credential and
/// goes.
const SENSITIVE_NAMES: &[&str] = &[
    "authorization",
    "body",
    "cookie",
    "credential",
    "credentials",
    "date_of_birth",
    "dob",
    "email",
    "email_address",
    "external_reference",
    "jwt",
    "national_id",
    "pass",
    "passwd",
    "password",
    "password_hash",
    "payload",
    "personal_id",
    "phone",
    "request_body",
    "response_body",
    "secret",
    "session",
    "set_cookie",
    "signature",
    "ssn",
    "token",
];

/// The same things under a qualified name, such as `internal_token`. None
/// of these matches an identifier: `token_id` and `job_id` are untouched.
const SENSITIVE_SUFFIXES: &[&str] = &[
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
];

/// Reports whether a field name must never carry its value into the logs.
pub(crate) fn is_sensitive(name: &str) -> bool {
    let lower = name.trim().to_ascii_lowercase();
    SENSITIVE_NAMES.contains(&lower.as_str())
        || SENSITIVE_SUFFIXES
            .iter()
            .any(|suffix| lower.ends_with(suffix))
}

/// Applies both rules to a whole record, at every depth. A credential is no
/// less a credential for being nested inside an object.
pub(crate) fn record(map: &mut Map<String, Value>) {
    for (key, value) in map.iter_mut() {
        if is_sensitive(key) {
            *value = Value::String(REDACTED.to_owned());
            continue;
        }
        clean(value);
    }
}

fn clean(value: &mut Value) {
    match value {
        Value::String(text) => {
            if let Some(replacement) = scrub(text) {
                *text = replacement;
            }
        }
        Value::Object(map) => record(map),
        Value::Array(items) => items.iter_mut().for_each(clean),
        _ => {}
    }
}

/// Cleans one string: a token found anywhere inside it is removed and what
/// remains is bounded. Returns `None` when nothing had to change, so an
/// untouched record costs no allocation.
pub(crate) fn scrub(value: &str) -> Option<String> {
    let without_tokens = if value.contains("eyJ") {
        remove_tokens(value)
    } else {
        None
    };
    let base = without_tokens.as_deref().unwrap_or(value);
    truncate(base).or(without_tokens)
}

/// Replaces every JSON Web Token in the string.
///
/// A token is recognised by shape rather than by the field it arrived in,
/// so one echoed into an error message or a URL is caught too. The scan
/// anchors on `eyJ`, the base64url encoding of a JSON object's first two
/// characters, which every JWT header begins with. Anchoring is what keeps
/// it precise: a hostname, a version or a file path cannot match.
fn remove_tokens(value: &str) -> Option<String> {
    let mut out = String::with_capacity(value.len());
    let mut rest = value;
    let mut found = false;

    while let Some(start) = rest.find("eyJ") {
        let (before, candidate) = rest.split_at(start);
        let end = token_length(candidate);
        match end {
            Some(length) => {
                out.push_str(before);
                out.push_str(REDACTED);
                rest = &candidate[length..];
                found = true;
            }
            None => {
                // Not a token: keep "eyJ" and carry on past it, so a later
                // occurrence in the same string is still examined.
                out.push_str(&rest[..start + 3]);
                rest = &rest[start + 3..];
            }
        }
    }
    if !found {
        return None;
    }
    out.push_str(rest);
    Some(out)
}

/// Length of the token starting at the front of `candidate`, or `None` when
/// what starts there is not one.
fn token_length(candidate: &str) -> Option<usize> {
    let run: &str = candidate
        .split(|c: char| !(c.is_ascii_alphanumeric() || c == '_' || c == '-' || c == '.'))
        .next()
        .unwrap_or("");

    let mut parts = run.split('.');
    let header = parts.next()?;
    let payload = parts.next()?;
    let signature = parts.next()?;
    // A header of "eyJ" plus at least four more characters, and a payload
    // of its own, is a token rather than a dotted word.
    if header.len() < 7 || payload.len() < 4 {
        return None;
    }
    let length = header.len() + 1 + payload.len() + 1 + signature.len();
    Some(length)
}

/// Bounds a string and says how much it dropped, so a reader can tell a
/// short value from a shortened one. The cut lands on a character boundary.
fn truncate(value: &str) -> Option<String> {
    if value.len() <= MAX_VALUE_LENGTH {
        return None;
    }
    let cut = (0..=MAX_VALUE_LENGTH)
        .rev()
        .find(|&i| value.is_char_boundary(i))
        .unwrap_or(0);
    let dropped = value.len() - cut;
    Some(format!("{}…[{dropped} more bytes omitted]", &value[..cut]))
}

/// A value with a real token's exact shape, and a credential for nothing.
///
/// It is assembled rather than written out as a literal so that no source
/// file in this repository contains a token-shaped string. A secret scanner
/// should flag every one it finds, and a fixture that had to be allow-listed
/// would blunt that for the next real one.
#[cfg(test)]
pub(crate) fn fake_jwt() -> String {
    // Base64url of {"alg":"HS256","typ":"JWT"} and {"sub":"not-a-real-user"},
    // each without its leading "ey", plus a signature that decodes to text.
    const HEADER: &str = "JhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9";
    const PAYLOAD: &str = "JzdWIiOiJub3QtYS1yZWFsLXVzZXIifQ";
    const SIGNATURE: &str = "bm90LWEtcmVhbC1zaWduYXR1cmU";
    format!("ey{HEADER}.ey{PAYLOAD}.{SIGNATURE}")
}

#[cfg(test)]
mod tests {
    use super::*;
    use serde_json::json;

    fn redacted(value: Value) -> Value {
        let mut map = match value {
            Value::Object(map) => map,
            other => panic!("not an object: {other}"),
        };
        record(&mut map);
        Value::Object(map)
    }

    #[test]
    fn credentials_are_removed_by_field_name() {
        let out = redacted(json!({
            "password": "hunter2",
            "token": "abcdef",
            "internal_token": "shared-secret",
            "authorization": "Bearer abc",
            "email": "someone@example.com",
        }));
        for field in [
            "password",
            "token",
            "internal_token",
            "authorization",
            "email",
        ] {
            assert_eq!(out[field], json!(REDACTED), "{field} was not redacted");
        }
        let line = out.to_string();
        for leaked in ["hunter2", "shared-secret", "someone@example.com"] {
            assert!(!line.contains(leaked), "{leaked} survived in {line}");
        }
    }

    #[test]
    fn identifiers_and_counts_are_kept() {
        let out = redacted(json!({
            "job_id": "job-1",
            "request_id": "req-1",
            "correlation_id": "corr-1",
            "token_id": "jti-1",
            "readings": 5000,
            "accepted": 4999,
            "path": "/internal/v1/process",
        }));
        assert_eq!(out["job_id"], json!("job-1"));
        assert_eq!(out["request_id"], json!("req-1"));
        assert_eq!(out["correlation_id"], json!("corr-1"));
        assert_eq!(out["token_id"], json!("jti-1"));
        assert_eq!(out["readings"], json!(5000));
        assert_eq!(out["accepted"], json!(4999));
        assert_eq!(out["path"], json!("/internal/v1/process"));
    }

    #[test]
    fn a_token_is_removed_whatever_it_is_called() {
        let token = fake_jwt();
        let out = redacted(json!({
            "message": format!("rejected {token} from upstream"),
            "value": token,
            "header": format!("Bearer {token}"),
            "url": format!("https://example.test/cb?access={token}&next=/x"),
        }));
        let line = out.to_string();
        assert!(!line.contains(&token), "a token survived in {line}");
        assert!(
            line.contains(REDACTED),
            "nothing was marked redacted: {line}"
        );
        // The text around the token is kept, or the record loses its point.
        assert!(line.contains("from upstream"), "{line}");
        assert!(line.contains("next=/x"), "{line}");
    }

    #[test]
    fn ordinary_values_are_left_alone() {
        for value in [
            "processor.vitalmesh.internal",
            "1.2.3",
            "/internal/v1/jobs/job-1",
            "connection refused: dial tcp 127.0.0.1:6379",
            "eyJ",
            "eyJshort.a.b",
        ] {
            let out = redacted(json!({ "detail": value }));
            assert_eq!(out["detail"], json!(value), "{value} was altered");
        }
    }

    #[test]
    fn an_oversized_value_is_truncated() {
        let payload = "A".repeat(MAX_VALUE_LENGTH * 3);
        let out = redacted(json!({ "detail": payload }));
        let got = out["detail"].as_str().unwrap();
        assert!(got.len() < payload.len(), "the value was not truncated");
        assert!(
            got.contains("more bytes omitted"),
            "a truncated value must say so: {got}"
        );
    }

    #[test]
    fn truncation_never_splits_a_character() {
        // Multi-byte characters straddle the bound wherever it falls.
        let payload = "é".repeat(MAX_VALUE_LENGTH);
        let out = redacted(json!({ "detail": payload }));
        // Reaching here at all means no panic; the value must still be text.
        assert!(out["detail"].as_str().unwrap().contains("omitted"));
    }

    #[test]
    fn nested_values_are_redacted_too() {
        let token = fake_jwt();
        let out = redacted(json!({
            "outer": {"password": "hunter2", "inner": [{"token": "abc"}, token]},
        }));
        let line = out.to_string();
        assert!(!line.contains("hunter2"), "{line}");
        assert!(!line.contains(&token), "{line}");
    }

    #[test]
    fn names_are_matched_case_insensitively() {
        for name in ["Password", "AUTHORIZATION", " Token ", "Internal_Token"] {
            assert!(is_sensitive(name), "{name} should be sensitive");
        }
        for name in ["token_id", "job_id", "readings", "path", "status"] {
            assert!(!is_sensitive(name), "{name} should be kept");
        }
    }
}
