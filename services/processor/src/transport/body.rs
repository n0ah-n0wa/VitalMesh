//! Strict request parsing.
//!
//! The contract draws a sharp line: a request that could not be understood is
//! `400 INVALID_REQUEST`, and only a well-formed request that breaks a rule is
//! `422`. Everything here is on the `400` side, and every rejection carries
//! the same code, because a client cannot act on the difference between a
//! misplaced brace and an unknown field.
//!
//! Deserialisation failures never reach the client verbatim. Serde messages
//! quote the offending input, which for this service is patient data; the
//! cause is kept for the log and the client is told which part of the
//! request was at fault.

use axum::http::{HeaderMap, header};
use serde::de::DeserializeOwned;

use crate::error::{Error, Kind, Result};

/// Rejects a request that is not `application/json`. A `charset` parameter is
/// accepted, since it says nothing this service disagrees with.
pub fn require_json(headers: &HeaderMap) -> Result<()> {
    let content_type = headers
        .get(header::CONTENT_TYPE)
        .and_then(|value| value.to_str().ok())
        .unwrap_or_default();
    let media_type = content_type
        .split(';')
        .next()
        .unwrap_or_default()
        .trim()
        .to_ascii_lowercase();
    if media_type == "application/json" {
        return Ok(());
    }
    Err(Error::new(
        Kind::UnsupportedMedia,
        "UNSUPPORTED_MEDIA_TYPE",
        "The request body must be application/json.",
    ))
}

/// Parses a JSON body into `T`, rejecting anything the type does not accept:
/// malformed JSON, an unknown field, a value of the wrong type, and every
/// domain rule the type enforces on construction.
pub fn parse<T: DeserializeOwned>(payload: &[u8]) -> Result<T> {
    serde_json::from_slice(payload).map_err(|cause| {
        Error::new(
            Kind::Invalid,
            "INVALID_REQUEST",
            "The request body is not a valid processing request.",
        )
        .with_source(cause)
    })
}

#[cfg(test)]
mod tests {
    use super::*;
    use axum::http::HeaderValue;
    use serde::Deserialize;

    fn content_type(value: &str) -> HeaderMap {
        let mut headers = HeaderMap::new();
        headers.insert(header::CONTENT_TYPE, HeaderValue::from_str(value).unwrap());
        headers
    }

    #[test]
    fn json_is_accepted_with_or_without_parameters() {
        for value in [
            "application/json",
            "application/json; charset=utf-8",
            "Application/JSON",
            "application/json ; charset=UTF-8",
        ] {
            require_json(&content_type(value)).unwrap_or_else(|e| panic!("{value:?}: {e}"));
        }
    }

    #[test]
    fn anything_else_is_an_unsupported_media_type() {
        for value in [
            "text/plain",
            "application/xml",
            "",
            "application/json-patch",
        ] {
            let error = require_json(&content_type(value))
                .expect_err(&format!("{value:?} should be rejected"));
            assert_eq!(error.code(), "UNSUPPORTED_MEDIA_TYPE");
            assert_eq!(error.kind(), Kind::UnsupportedMedia, "{value:?}");
        }
        let error = require_json(&HeaderMap::new()).expect_err("a missing type is rejected");
        assert_eq!(error.code(), "UNSUPPORTED_MEDIA_TYPE");
    }

    #[derive(Debug, Deserialize)]
    #[serde(deny_unknown_fields)]
    struct Sample {
        wanted: u8,
    }

    #[test]
    fn a_valid_body_parses() {
        let parsed: Sample = parse(br#"{"wanted": 7}"#).unwrap();
        assert_eq!(parsed.wanted, 7);
    }

    #[test]
    fn every_unparseable_body_is_one_invalid_request() {
        for body in [
            &br#"{"wanted": }"#[..],         // malformed JSON
            br#"{"wanted": 7, "extra": 1}"#, // unknown field
            br#"{"wanted": "seven"}"#,       // wrong type
            br#"{}"#,                        // missing field
            br#"{"wanted": 300}"#,           // out of range for the type
            b"",                             // empty
            b"[]",                           // wrong shape
        ] {
            let error = parse::<Sample>(body).expect_err("must be rejected");
            assert_eq!(error.kind(), Kind::Invalid);
            assert_eq!(error.code(), "INVALID_REQUEST");
        }
    }

    /// Serde quotes the input it choked on. That input is patient data, so it
    /// must stay in the source, which only the log sees.
    #[test]
    fn the_offending_input_never_reaches_the_message() {
        let error = parse::<Sample>(br#"{"wanted": "secret-value"}"#).unwrap_err();
        assert!(
            !error.message().contains("secret-value"),
            "message leaked the payload: {}",
            error.message()
        );
        assert!(
            std::error::Error::source(&error).is_some(),
            "the cause must be kept for the log"
        );
    }
}
