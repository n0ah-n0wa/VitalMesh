use axum::Json;
use axum::http::{HeaderValue, StatusCode, header};
use axum::response::{IntoResponse, Response};
use serde::{Deserialize, Serialize};

use crate::error::{Error, Kind};
use crate::transport::context::current_request_id;

/// The envelope returned for every failed request.
#[derive(Debug, Clone, Serialize, Deserialize, PartialEq, Eq)]
pub struct ErrorResponse {
    pub error: ErrorBody,
}

/// One failure.
#[derive(Debug, Clone, Serialize, Deserialize, PartialEq, Eq)]
pub struct ErrorBody {
    pub code: String,
    pub message: String,
    pub request_id: String,
}

/// Builds an envelope response with an explicit status. The request id comes
/// from the request being handled on the current task.
pub(super) fn envelope(status: StatusCode, code: &str, message: &str) -> Response {
    let body = ErrorResponse {
        error: ErrorBody {
            code: code.to_owned(),
            message: message.to_owned(),
            request_id: current_request_id(),
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

impl IntoResponse for Error {
    fn into_response(self) -> Response {
        if self.kind() == Kind::Internal {
            tracing::error!(error = %self, "request failed");
        }

        let mut response = envelope(status_for(self.kind()), self.code(), self.message());
        if self.kind() == Kind::Overloaded {
            response
                .headers_mut()
                .insert(header::RETRY_AFTER, HeaderValue::from_static("1"));
        }
        response
    }
}
