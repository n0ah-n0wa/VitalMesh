use std::fmt;

use crate::error::{Error, Kind};

/// Why a domain value was rejected. Every variant has a stable code and a
/// message that is safe to return to clients.
#[derive(Debug, Clone, PartialEq, Eq)]
pub enum DomainError {
    InvalidIdentifier {
        what: &'static str,
        reason: &'static str,
    },
    NonFiniteValue {
        value: String,
    },
    UnitMismatch {
        measurement_type: &'static str,
        expected: &'static str,
        actual: &'static str,
    },
    ValueOutOfRange {
        measurement_type: &'static str,
        value: String,
        min: String,
        max: String,
    },
    InvalidTimestamp {
        input: String,
        reason: String,
    },
    TimestampTooFarInFuture {
        timestamp: String,
        limit: String,
    },
    TimestampTooOld {
        timestamp: String,
        limit: String,
    },
    TimestampOrder {
        what: &'static str,
    },
    EmptyBatch,
    BatchTooLarge {
        size: usize,
        max: usize,
    },
    InvalidTransition {
        from: &'static str,
        to: &'static str,
    },
    InvalidPercentile {
        value: u8,
    },
    InvalidAlgorithmVersion {
        input: String,
    },
    EmptyParameters {
        what: &'static str,
    },
    InconsistentJob {
        reason: &'static str,
    },
    InconsistentStatistics {
        reason: &'static str,
    },
    InconsistentResult {
        reason: &'static str,
    },
}

impl DomainError {
    /// Stable machine-readable code.
    pub fn code(&self) -> &'static str {
        match self {
            Self::InvalidIdentifier { .. } => "INVALID_IDENTIFIER",
            Self::NonFiniteValue { .. } => "NON_FINITE_VALUE",
            Self::UnitMismatch { .. } => "UNIT_MISMATCH",
            Self::ValueOutOfRange { .. } => "VALUE_OUT_OF_RANGE",
            Self::InvalidTimestamp { .. } => "INVALID_TIMESTAMP",
            Self::TimestampTooFarInFuture { .. } => "TIMESTAMP_IN_FUTURE",
            Self::TimestampTooOld { .. } => "TIMESTAMP_TOO_OLD",
            Self::TimestampOrder { .. } => "TIMESTAMP_ORDER",
            Self::EmptyBatch => "EMPTY_BATCH",
            Self::BatchTooLarge { .. } => "BATCH_TOO_LARGE",
            Self::InvalidTransition { .. } => "INVALID_JOB_TRANSITION",
            Self::InvalidPercentile { .. } => "INVALID_PERCENTILE",
            Self::InvalidAlgorithmVersion { .. } => "INVALID_ALGORITHM_VERSION",
            Self::EmptyParameters { .. } => "EMPTY_PARAMETERS",
            Self::InconsistentJob { .. } => "INCONSISTENT_JOB",
            Self::InconsistentStatistics { .. } => "INCONSISTENT_STATISTICS",
            Self::InconsistentResult { .. } => "INCONSISTENT_RESULT",
        }
    }

    fn kind(&self) -> Kind {
        match self {
            Self::InvalidTransition { .. } => Kind::Conflict,
            _ => Kind::Validation,
        }
    }
}

impl fmt::Display for DomainError {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        match self {
            Self::InvalidIdentifier { what, reason } => write!(f, "invalid {what}: {reason}"),
            Self::NonFiniteValue { value } => write!(f, "value {value} is not a finite number"),
            Self::UnitMismatch {
                measurement_type,
                expected,
                actual,
            } => write!(
                f,
                "{measurement_type} must use unit {expected}, got {actual}"
            ),
            Self::ValueOutOfRange {
                measurement_type,
                value,
                min,
                max,
            } => write!(
                f,
                "{measurement_type} value {value} is outside the technical range {min} to {max}"
            ),
            Self::InvalidTimestamp { input, reason } => {
                write!(f, "invalid timestamp {input:?}: {reason}")
            }
            Self::TimestampTooFarInFuture { timestamp, limit } => {
                write!(
                    f,
                    "timestamp {timestamp} is later than the accepted limit {limit}"
                )
            }
            Self::TimestampTooOld { timestamp, limit } => {
                write!(
                    f,
                    "timestamp {timestamp} is earlier than the accepted limit {limit}"
                )
            }
            Self::TimestampOrder { what } => write!(f, "timestamps out of order: {what}"),
            Self::EmptyBatch => f.write_str("a batch must contain at least one measurement"),
            Self::BatchTooLarge { size, max } => write!(
                f,
                "batch of {size} measurements exceeds the maximum of {max}"
            ),
            Self::InvalidTransition { from, to } => {
                write!(f, "a job cannot move from {from} to {to}")
            }
            Self::InvalidPercentile { value } => {
                write!(f, "percentile {value} is not between 1 and 99")
            }
            Self::InvalidAlgorithmVersion { input } => {
                write!(
                    f,
                    "algorithm version {input:?} is not of the form major.minor.patch"
                )
            }
            Self::EmptyParameters { what } => write!(f, "at least one {what} is required"),
            Self::InconsistentJob { reason } => write!(f, "inconsistent job: {reason}"),
            Self::InconsistentStatistics { reason } => {
                write!(f, "inconsistent statistics: {reason}")
            }
            Self::InconsistentResult { reason } => write!(f, "inconsistent result: {reason}"),
        }
    }
}

impl std::error::Error for DomainError {}

impl From<DomainError> for Error {
    fn from(err: DomainError) -> Self {
        Error::new(err.kind(), err.code(), err.to_string())
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn converts_into_the_service_error_with_code_and_kind() {
        let validation: Error = DomainError::EmptyBatch.into();
        assert_eq!(validation.kind(), Kind::Validation);
        assert_eq!(validation.code(), "EMPTY_BATCH");
        assert_eq!(
            validation.message(),
            "a batch must contain at least one measurement"
        );

        let conflict: Error = DomainError::InvalidTransition {
            from: "COMPLETED",
            to: "PROCESSING",
        }
        .into();
        assert_eq!(conflict.kind(), Kind::Conflict);
        assert_eq!(conflict.code(), "INVALID_JOB_TRANSITION");
    }
}
