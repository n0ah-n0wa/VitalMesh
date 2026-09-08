use axum::Json;
use axum::http::{HeaderValue, StatusCode, header};
use axum::response::{IntoResponse, Response};
use serde::{Deserialize, Serialize};

use crate::error::{Error, Kind};
use crate::transport::context::current_context;

/// The envelope returned for every failed request.
#[derive(Debug, Clone, Serialize, Deserialize, PartialEq, Eq)]
pub struct ErrorResponse {
    pub error: ErrorBody,
}

/// One failure. `code` is stable and is what a client branches on; `message`
/// is a safe sentence for logs and operators. `retryable` states whether
/// repeating the identical request could succeed, so a client holding only
/// the body can classify the failure (SPECIFICATIONS.md section 93).
#[derive(Debug, Clone, Serialize, Deserialize, PartialEq, Eq)]
pub struct ErrorBody {
    pub code: String,
    pub message: String,
    pub request_id: String,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub correlation_id: Option<String>,
    pub retryable: bool,
    /// Structured, non-sensitive context, such as a limit that was exceeded.
    #[serde(skip_serializing_if = "Option::is_none")]
    pub details: Option<serde_json::Map<String, serde_json::Value>>,
}

/// Builds an envelope response with an explicit status. The identifiers come
/// from the request being handled on the current task.
pub(super) fn envelope(status: StatusCode, code: &str, message: &str, retryable: bool) -> Response {
    envelope_with_details(status, code, message, retryable, None)
}

fn envelope_with_details(
    status: StatusCode,
    code: &str,
    message: &str,
    retryable: bool,
    details: Option<serde_json::Map<String, serde_json::Value>>,
) -> Response {
    let context = current_context();
    let body = ErrorResponse {
        error: ErrorBody {
            code: code.to_owned(),
            message: message.to_owned(),
            request_id: context
                .as_ref()
                .map(|c| c.request_id.clone())
                .unwrap_or_default(),
            correlation_id: context.map(|c| c.correlation_id),
            retryable,
            details,
        },
    };
    (status, Json(body)).into_response()
}

pub(super) fn status_for(kind: Kind) -> StatusCode {
    match kind {
        Kind::Internal => StatusCode::INTERNAL_SERVER_ERROR,
        Kind::Invalid => StatusCode::BAD_REQUEST,
        Kind::Validation => StatusCode::UNPROCESSABLE_ENTITY,
        Kind::NotFound => StatusCode::NOT_FOUND,
        Kind::Conflict => StatusCode::CONFLICT,
        Kind::Overloaded | Kind::Cancelled | Kind::Unavailable => StatusCode::SERVICE_UNAVAILABLE,
        Kind::Timeout => StatusCode::GATEWAY_TIMEOUT,
        Kind::Unauthenticated => StatusCode::UNAUTHORIZED,
        Kind::UnsupportedMedia => StatusCode::UNSUPPORTED_MEDIA_TYPE,
    }
}

/// The envelope code for a framework-generated status, used when axum
/// rejects a request before a handler runs.
pub(super) fn code_for_status(status: StatusCode) -> String {
    match status {
        StatusCode::BAD_REQUEST => "INVALID_REQUEST".to_owned(),
        StatusCode::PAYLOAD_TOO_LARGE => "REQUEST_BODY_TOO_LARGE".to_owned(),
        StatusCode::UNSUPPORTED_MEDIA_TYPE => "UNSUPPORTED_MEDIA_TYPE".to_owned(),
        StatusCode::UNPROCESSABLE_ENTITY => "VALIDATION_FAILED".to_owned(),
        other => other
            .canonical_reason()
            .unwrap_or("ERROR")
            .to_ascii_uppercase()
            .replace(['-', ' '], "_"),
    }
}

/// Whether a framework-generated status is worth retrying. Only a server
/// running out of capacity is; every rejection of the request itself is the
/// client's to fix.
pub(super) fn retryable_status(status: StatusCode) -> bool {
    status.is_server_error() && status != StatusCode::NOT_IMPLEMENTED
}

impl IntoResponse for Error {
    fn into_response(self) -> Response {
        if self.kind() == Kind::Internal {
            tracing::error!(error = %self, "request failed");
        }

        let mut response = envelope_with_details(
            status_for(self.kind()),
            self.code(),
            self.message(),
            self.is_retryable(),
            self.details().cloned(),
        );
        if self.kind() == Kind::Overloaded {
            response
                .headers_mut()
                .insert(header::RETRY_AFTER, HeaderValue::from_static("1"));
        }
        response
    }
}
