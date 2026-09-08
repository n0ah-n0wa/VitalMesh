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
    /// The caller did not present a valid internal credential.
    Unauthenticated,
    /// The request body is not in a media type this service accepts.
    UnsupportedMedia,
}

impl Kind {
    /// Whether repeating the identical request could succeed
    /// (SPECIFICATIONS.md section 93). A caller uses this to decide whether
    /// to spend a retry; it is reported to clients as `error.retryable`.
    ///
    /// A cancellation is *not* retryable: the work stopped because someone
    /// asked it to, and repeating it would undo that.
    pub fn is_retryable(self) -> bool {
        match self {
            Self::Internal | Self::Overloaded | Self::Timeout | Self::Unavailable => true,
            Self::Invalid
            | Self::Validation
            | Self::NotFound
            | Self::Conflict
            | Self::Cancelled
            | Self::Unauthenticated
            | Self::UnsupportedMedia => false,
        }
    }
}

type Source = Box<dyn std::error::Error + Send + Sync + 'static>;

/// A classified error with a stable code and a client-safe message.
#[derive(Debug)]
pub struct Error {
    kind: Kind,
    code: &'static str,
    message: String,
    /// Structured, non-sensitive context, such as the limit a request
    /// exceeded. It is returned to clients, so nothing internal goes here.
    details: Option<serde_json::Map<String, serde_json::Value>>,
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
            details: None,
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

    pub fn unauthenticated() -> Self {
        Self::new(
            Kind::Unauthenticated,
            "UNAUTHENTICATED",
            "Authentication is required.",
        )
    }

    /// Attaches the underlying cause.
    pub fn with_source(mut self, source: impl std::error::Error + Send + Sync + 'static) -> Self {
        self.source = Some(Box::new(source));
        self
    }

    /// Attaches structured context returned to the client alongside the
    /// code. Only non-sensitive facts belong here, such as a limit that was
    /// exceeded and the value that exceeded it.
    pub fn with_details<K, V>(mut self, details: impl IntoIterator<Item = (K, V)>) -> Self
    where
        K: Into<String>,
        V: Into<serde_json::Value>,
    {
        self.details = Some(
            details
                .into_iter()
                .map(|(k, v)| (k.into(), v.into()))
                .collect(),
        );
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

    pub fn details(&self) -> Option<&serde_json::Map<String, serde_json::Value>> {
        self.details.as_ref()
    }

    /// Whether repeating the identical request could succeed.
    pub fn is_retryable(&self) -> bool {
        self.kind.is_retryable()
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
        assert_eq!(Error::unauthenticated().kind(), Kind::Unauthenticated);
    }

    /// A client spends retries on the strength of this classification, so
    /// every kind states its answer rather than inheriting a default.
    #[test]
    fn retry_classification_follows_the_specification() {
        for kind in [
            Kind::Internal,
            Kind::Overloaded,
            Kind::Timeout,
            Kind::Unavailable,
        ] {
            assert!(kind.is_retryable(), "{kind:?} should be retryable");
        }
        for kind in [
            Kind::Invalid,
            Kind::Validation,
            Kind::NotFound,
            Kind::Conflict,
            Kind::Cancelled,
            Kind::Unauthenticated,
            Kind::UnsupportedMedia,
        ] {
            assert!(!kind.is_retryable(), "{kind:?} should not be retryable");
        }
    }

    #[test]
    fn details_are_optional_and_carried_verbatim() {
        let plain = Error::overloaded();
        assert!(plain.details().is_none());

        let detailed = Error::new(Kind::Validation, "JOB_TOO_LARGE", "Too large.")
            .with_details([("maximum", 10_i64), ("received", 11)]);
        let details = detailed.details().expect("details");
        assert_eq!(details["maximum"], serde_json::json!(10));
        assert_eq!(details["received"], serde_json::json!(11));
        // Details never reach the message, which stays a plain sentence.
        assert_eq!(detailed.message(), "Too large.");
    }
}
