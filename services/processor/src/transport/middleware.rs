//! Cross-cutting request handling: identification, access logging, envelope
//! mapping for framework rejections, and the request timeout.

use std::time::{Duration, Instant};

use axum::extract::{Request, State};
use axum::http::{HeaderMap, HeaderValue, header};
use axum::middleware::Next;
use axum::response::{IntoResponse, Response};
use tracing::Instrument;

use crate::error::Error;
use crate::requestid::{self, CORRELATION_ID_HEADER, REQUEST_ID_HEADER};
use crate::transport::context::{self, RequestContext};
use crate::transport::error;

/// Assigns request and correlation ids, makes them available to handlers,
/// echoes them in the response, and logs one record per request.
pub async fn request_context(request: Request, next: Next) -> Response {
    let request_id = header_value(request.headers(), REQUEST_ID_HEADER)
        .filter(|v| requestid::is_valid(v))
        .map(str::to_owned)
        .unwrap_or_else(requestid::generate);
    let correlation_id = header_value(request.headers(), CORRELATION_ID_HEADER)
        .filter(|v| requestid::is_valid(v))
        .map(str::to_owned)
        .unwrap_or_else(|| request_id.clone());

    let span = tracing::info_span!(
        "request",
        request_id = %request_id,
        correlation_id = %correlation_id,
        method = %request.method(),
        path = %request.uri().path(),
    );
    let ids = RequestContext {
        request_id,
        correlation_id,
    };

    let start = Instant::now();
    let mut response = context::scope(ids.clone(), next.run(request))
        .instrument(span.clone())
        .await;

    span.in_scope(|| {
        tracing::info!(
            status = response.status().as_u16(),
            duration_ms = start.elapsed().as_millis() as u64,
            "request"
        );
    });

    set_header(response.headers_mut(), REQUEST_ID_HEADER, &ids.request_id);
    set_header(
        response.headers_mut(),
        CORRELATION_ID_HEADER,
        &ids.correlation_id,
    );
    response
}

/// Rewrites error responses produced by the framework itself (for example a
/// body-limit or content-type rejection, which axum answers in plain text)
/// into the JSON error envelope, so clients see one error shape everywhere.
pub async fn envelope_rejections(request: Request, next: Next) -> Response {
    let response = next.run(request).await;
    let status = response.status();
    if !(status.is_client_error() || status.is_server_error()) || is_json(response.headers()) {
        return response;
    }
    let code = error::code_for_status(status);
    let message = format!(
        "{}.",
        status.canonical_reason().unwrap_or("The request failed")
    );
    error::envelope(status, &code, &message)
}

/// Fails the request with a timeout error if the handler exceeds `limit`.
/// The handler future is dropped, which cancels its work.
pub async fn timeout(State(limit): State<Duration>, request: Request, next: Next) -> Response {
    match tokio::time::timeout(limit, next.run(request)).await {
        Ok(response) => response,
        Err(_elapsed) => Error::timeout().into_response(),
    }
}

fn is_json(headers: &HeaderMap) -> bool {
    header_value(headers, header::CONTENT_TYPE.as_str())
        .is_some_and(|ct| ct.starts_with("application/json"))
}

fn header_value<'a>(headers: &'a HeaderMap, name: &str) -> Option<&'a str> {
    headers.get(name).and_then(|v| v.to_str().ok())
}

fn set_header(headers: &mut HeaderMap, name: &'static str, value: &str) {
    // Values are validated identifiers, so this cannot fail; if it ever did,
    // omitting the header is the safe outcome.
    if let Ok(value) = HeaderValue::from_str(value) {
        headers.insert(name, value);
    }
}
