//! Algorithm versions, recorded on every result for reproducibility.

use std::fmt;
use std::str::FromStr;

use serde::{Deserialize, Serialize};

use super::error::DomainError;

/// A `major.minor.patch` version of the processing algorithms. Results
/// produced under different versions are never compared as equal.
#[derive(Debug, Clone, Copy, PartialEq, Eq, PartialOrd, Ord, Hash, Serialize, Deserialize)]
#[serde(try_from = "String", into = "String")]
pub struct AlgorithmVersion {
    major: u16,
    minor: u16,
    patch: u16,
}

impl AlgorithmVersion {
    pub const fn new(major: u16, minor: u16, patch: u16) -> Self {
        Self {
            major,
            minor,
            patch,
        }
    }

    pub fn major(self) -> u16 {
        self.major
    }

    pub fn minor(self) -> u16 {
        self.minor
    }

    pub fn patch(self) -> u16 {
        self.patch
    }
}

impl FromStr for AlgorithmVersion {
    type Err = DomainError;

    /// Accepts exactly three dot-separated decimal numbers without signs,
    /// spaces or leading zeros (other than `0` itself).
    fn from_str(raw: &str) -> Result<Self, DomainError> {
        let invalid = || DomainError::InvalidAlgorithmVersion {
            input: raw.to_owned(),
        };
        let mut parts = raw.split('.');
        let mut next = || -> Result<u16, DomainError> {
            let part = parts.next().ok_or_else(invalid)?;
            let canonical =
                part.bytes().all(|b| b.is_ascii_digit()) && (part == "0" || !part.starts_with('0'));
            if !canonical {
                return Err(invalid());
            }
            part.parse().map_err(|_| invalid())
        };
        let version = Self::new(next()?, next()?, next()?);
        if parts.next().is_some() {
            return Err(invalid());
        }
        Ok(version)
    }
}

impl TryFrom<String> for AlgorithmVersion {
    type Error = DomainError;

    fn try_from(raw: String) -> Result<Self, DomainError> {
        raw.parse()
    }
}

impl From<AlgorithmVersion> for String {
    fn from(version: AlgorithmVersion) -> Self {
        version.to_string()
    }
}

impl fmt::Display for AlgorithmVersion {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        write!(f, "{}.{}.{}", self.major, self.minor, self.patch)
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn parses_and_displays_canonical_versions() {
        for (raw, want) in [
            ("0.0.0", AlgorithmVersion::new(0, 0, 0)),
            ("1.2.3", AlgorithmVersion::new(1, 2, 3)),
            ("65535.0.10", AlgorithmVersion::new(65535, 0, 10)),
        ] {
            let parsed: AlgorithmVersion = raw.parse().unwrap();
            assert_eq!(parsed, want);
            assert_eq!(parsed.to_string(), raw);
        }
    }

    #[test]
    fn rejects_non_canonical_input() {
        for bad in [
            "",
            "1",
            "1.2",
            "1.2.3.4",
            "01.2.3",
            "1.02.3",
            "1.2.3 ",
            " 1.2.3",
            "v1.2.3",
            "1.2.-3",
            "1.2.x",
            "65536.0.0",
            "1..3",
            "1.2.",
        ] {
            let err = bad.parse::<AlgorithmVersion>().expect_err(bad);
            assert_eq!(err.code(), "INVALID_ALGORITHM_VERSION", "{bad}");
        }
    }

    #[test]
    fn orders_numerically_not_lexically() {
        let v1_10: AlgorithmVersion = "1.10.0".parse().unwrap();
        let v1_9: AlgorithmVersion = "1.9.0".parse().unwrap();
        let v2: AlgorithmVersion = "2.0.0".parse().unwrap();
        assert!(v1_9 < v1_10 && v1_10 < v2);
    }

    #[test]
    fn serde_uses_the_string_form() {
        let v = AlgorithmVersion::new(1, 4, 2);
        assert_eq!(serde_json::to_string(&v).unwrap(), "\"1.4.2\"");
        assert_eq!(
            serde_json::from_str::<AlgorithmVersion>("\"1.4.2\"").unwrap(),
            v
        );
        let err = serde_json::from_str::<AlgorithmVersion>("\"1.4\"").unwrap_err();
        assert!(err.to_string().contains("major.minor.patch"), "{err}");
        assert!(serde_json::from_str::<AlgorithmVersion>("142").is_err());
    }
}
