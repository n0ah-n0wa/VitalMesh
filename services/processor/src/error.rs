//! The classified error type used across the service. Every error carries a
//! stable code and a client-safe message; the underlying cause, when any,
//! stays server-side.

use std::fmt;

/// Classification a transport maps to a status code.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum Kind {
    /// Unexpected failure. The cause is logged and never returned to clients.
    Internal,
    /// Malformed request.
    Invalid,
    /// Well-formed request that violates a rule.
    Validation,
    NotFound,
    Conflict,
    /// No processing capacity is available right now.
    Overloaded,
    /// The work exceeded its time bound.
    Timeout,
    /// The work was cancelled before it completed.
    Cancelled,
    /// The service cannot accept work, for example during shutdown.
    Unavailable,
}

type Source = Box<dyn std::error::Error + Send + Sync + 'static>;

/// A classified error with a stable code and a client-safe message.
#[derive(Debug)]
pub struct Error {
    kind: Kind,
    code: &'static str,
    message: String,
    source: Option<Source>,
}

/// Convenience alias used throughout the crate.
pub type Result<T> = std::result::Result<T, Error>;

impl Error {
    /// Builds an error of `kind` with a client-safe message.
    pub fn new(kind: Kind, code: &'static str, message: impl Into<String>) -> Self {
        Self {
            kind,
            code,
            message: message.into(),
            source: None,
        }
    }

    /// Wraps an unexpected failure. The message returned to clients is
    /// generic; `source` is kept for logs.
    pub fn internal(source: impl std::error::Error + Send + Sync + 'static) -> Self {
        Self::new(
            Kind::Internal,
            "INTERNAL_ERROR",
            "An internal error occurred.",
        )
        .with_source(source)
    }

    pub fn overloaded() -> Self {
        Self::new(
            Kind::Overloaded,
            "PROCESSOR_OVERLOADED",
            "The processor has no free capacity; retry later.",
        )
    }

    pub fn timeout() -> Self {
        Self::new(
            Kind::Timeout,
            "PROCESSING_TIMEOUT",
            "The work exceeded its time limit.",
        )
    }

    pub fn cancelled() -> Self {
        Self::new(
            Kind::Cancelled,
            "PROCESSING_CANCELLED",
            "The work was cancelled.",
        )
    }

    /// Attaches the underlying cause.
    pub fn with_source(mut self, source: impl std::error::Error + Send + Sync + 'static) -> Self {
        self.source = Some(Box::new(source));
        self
    }

    pub fn kind(&self) -> Kind {
        self.kind
    }

    pub fn code(&self) -> &'static str {
        self.code
    }

    pub fn message(&self) -> &str {
        &self.message
    }
}

impl fmt::Display for Error {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        write!(f, "{}: {}", self.code, self.message)?;
        if let Some(source) = &self.source {
            write!(f, ": {source}")?;
        }
        Ok(())
    }
}

impl std::error::Error for Error {
    fn source(&self) -> Option<&(dyn std::error::Error + 'static)> {
        self.source
            .as_deref()
            .map(|s| s as &(dyn std::error::Error + 'static))
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn display_includes_code_message_and_source() {
        let plain = Error::new(Kind::NotFound, "JOB_NOT_FOUND", "No such job.");
        assert_eq!(plain.to_string(), "JOB_NOT_FOUND: No such job.");

        let io = std::io::Error::other("disk on fire");
        let wrapped = Error::internal(io);
        assert_eq!(wrapped.kind(), Kind::Internal);
        assert_eq!(wrapped.message(), "An internal error occurred.");
        assert_eq!(
            wrapped.to_string(),
            "INTERNAL_ERROR: An internal error occurred.: disk on fire"
        );
        assert!(std::error::Error::source(&wrapped).is_some());
    }

    #[test]
    fn convenience_constructors_are_classified() {
        assert_eq!(Error::overloaded().kind(), Kind::Overloaded);
        assert_eq!(Error::timeout().kind(), Kind::Timeout);
        assert_eq!(Error::cancelled().kind(), Kind::Cancelled);
    }
}
