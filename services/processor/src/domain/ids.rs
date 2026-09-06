//! Opaque identifiers assigned by the gateway. The processor validates their
//! shape but does not depend on how they are generated.

use std::fmt;
use std::str::FromStr;

use serde::{Deserialize, Deserializer, Serialize};

use super::error::DomainError;

const MAX_LEN: usize = 128;

fn validate(raw: &str, what: &'static str) -> Result<(), DomainError> {
    let reject = |reason| DomainError::InvalidIdentifier { what, reason };
    if raw.is_empty() {
        return Err(reject("must not be empty"));
    }
    if raw.len() > MAX_LEN {
        return Err(reject("must be at most 128 characters"));
    }
    if !raw
        .bytes()
        .all(|b| b.is_ascii_alphanumeric() || matches!(b, b'.' | b'_' | b'-'))
    {
        return Err(reject("must contain only [A-Za-z0-9._-]"));
    }
    Ok(())
}

macro_rules! identifier {
    ($(#[$doc:meta])* $name:ident, $what:literal) => {
        $(#[$doc])*
        #[derive(Debug, Clone, PartialEq, Eq, Hash, PartialOrd, Ord, Serialize)]
        #[serde(transparent)]
        pub struct $name(String);

        impl $name {
            /// Accepts 1 to 128 characters from `[A-Za-z0-9._-]`.
            pub fn parse(raw: &str) -> Result<Self, DomainError> {
                validate(raw, $what).map(|()| Self(raw.to_owned()))
            }

            /// Like [`Self::parse`] but keeps the caller's allocation.
            pub fn from_string(raw: String) -> Result<Self, DomainError> {
                validate(&raw, $what).map(|()| Self(raw))
            }

            pub fn as_str(&self) -> &str {
                &self.0
            }
        }

        impl TryFrom<String> for $name {
            type Error = DomainError;

            fn try_from(raw: String) -> Result<Self, DomainError> {
                Self::from_string(raw)
            }
        }

        impl fmt::Display for $name {
            fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
                f.write_str(&self.0)
            }
        }

        impl FromStr for $name {
            type Err = DomainError;

            fn from_str(raw: &str) -> Result<Self, DomainError> {
                Self::parse(raw)
            }
        }

        impl<'de> Deserialize<'de> for $name {
            fn deserialize<D: Deserializer<'de>>(deserializer: D) -> Result<Self, D::Error> {
                let raw = String::deserialize(deserializer)?;
                Self::from_string(raw).map_err(serde::de::Error::custom)
            }
        }
    };
}

identifier!(
    /// Identifier of a processing job.
    JobId,
    "job id"
);
identifier!(
    /// Identifier of a synthetic patient.
    PatientId,
    "patient id"
);
identifier!(
    /// Identifier of a measurement.
    MeasurementId,
    "measurement id"
);

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn accepts_valid_identifiers() {
        for raw in ["a", "job-1_a.b", "0123456789abcdef-0123", &"x".repeat(128)] {
            assert_eq!(JobId::parse(raw).unwrap().as_str(), raw);
            assert_eq!(raw.parse::<PatientId>().unwrap().to_string(), raw);
            assert!(MeasurementId::parse(raw).is_ok());
        }
    }

    #[test]
    fn rejects_invalid_identifiers_with_the_reason() {
        let cases = [
            ("", "must not be empty"),
            (&"x".repeat(129), "must be at most 128 characters"),
            ("has space", "must contain only [A-Za-z0-9._-]"),
            ("semi;colon", "must contain only [A-Za-z0-9._-]"),
            ("ünïcode", "must contain only [A-Za-z0-9._-]"),
            ("new\nline", "must contain only [A-Za-z0-9._-]"),
        ];
        for (raw, want) in cases {
            let err = JobId::parse(raw).expect_err(raw);
            assert_eq!(
                err,
                DomainError::InvalidIdentifier {
                    what: "job id",
                    reason: want
                }
            );
            assert_eq!(err.code(), "INVALID_IDENTIFIER");
        }
    }

    #[test]
    fn serde_round_trip_and_rejection() {
        let id: PatientId = serde_json::from_str("\"p-1\"").unwrap();
        assert_eq!(id.as_str(), "p-1");
        assert_eq!(serde_json::to_string(&id).unwrap(), "\"p-1\"");

        let err = serde_json::from_str::<PatientId>("\"bad id\"").unwrap_err();
        assert!(err.to_string().contains("invalid patient id"), "{err}");
        assert!(serde_json::from_str::<PatientId>("42").is_err());
    }
}
