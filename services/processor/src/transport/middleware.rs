//! Cross-cutting request handling: identification, access logging, envelope
//! mapping for framework rejections, and the request timeout.

use std::time::{Duration, Instant};

use axum::extract::{MatchedPath, Request, State};
use axum::http::{HeaderMap, HeaderValue, StatusCode, header};
use axum::middleware::Next;
use axum::response::{IntoResponse, Response};
use tracing::Instrument;

use crate::config::Secret;
use crate::error::Error;
use crate::requestid::{self, CORRELATION_ID_HEADER, REQUEST_ID_HEADER};
use crate::state::SharedState;
use crate::transport::context::{self, RequestContext};
use crate::transport::error;

/// Reduces a request path to the route it names, so that nothing derived
/// from a path carries an identifier.
///
/// The table is this service's own routes, which are few and fixed. A path
/// matching none of them is "unmatched" rather than being repeated: an
/// unknown path is exactly the kind that might carry anything.
fn route_of(path: &str) -> &'static str {
    match path {
        "/health" => "/health",
        "/ready" => "/ready",
        "/metrics" => "/metrics",
        "/internal/v1/health" => "/internal/v1/health",
        "/internal/v1/process" => "/internal/v1/process",
        _ if path.starts_with("/internal/v1/jobs/") => "/internal/v1/jobs/{job_id}",
        _ => "unmatched",
    }
}

/// Attaches an OpenTelemetry parent context to a span, so a trace that
/// began in the gateway continues here. It is a separate function because
/// the trait it needs is only in scope for this line.
fn set_parent(span: &tracing::Span, parent: opentelemetry::Context) {
    use tracing_opentelemetry::OpenTelemetrySpanExt;
    // A parent that cannot be attached leaves this span the root of its own
    // trace, which is a worse trace but a served request. Nothing here is
    // worth failing a request over.
    let _ = span.set_parent(parent);
}

/// Records one served request: its count, its latency and, when it failed,
/// its class.
///
/// The route label is the pattern axum matched, never the request path. A
/// path carries identifiers, and a metric labelled by one grows without
/// bound and leaks what it names (SPECIFICATIONS.md section 41). A request
/// that matched nothing is recorded as "unmatched" rather than by its path.
pub async fn record_metrics(
    State(state): State<SharedState>,
    request: Request,
    next: Next,
) -> Response {
    let method = request.method().as_str().to_owned();
    let route = request
        .extensions()
        .get::<MatchedPath>()
        .map(|matched| matched.as_str().to_owned())
        .unwrap_or_else(|| "unmatched".to_owned());

    let started = Instant::now();
    let response = next.run(request).await;
    state.metrics.http_request(
        &method,
        &route,
        response.status().as_u16(),
        started.elapsed().as_secs_f64(),
    );
    response
}

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

    // W3C trace context, when the caller sent it, makes this span a child
    // of the gateway's rather than the root of a trace of its own.
    let parent = crate::tracing_otel::context_from_headers(request.headers());
    let trace_id = crate::tracing_otel::trace_id_from_headers(request.headers());

    // Two names for the same span, so the exporter can drop probes by name
    // (the one thing a filter can see before fields exist) while the log
    // formatter still records them. A liveness probe every few seconds per
    // replica is not an important request and would bury the ones that are.
    let path = request.uri().path();
    let route = route_of(path);
    let span = if crate::tracing_otel::is_probe(path) {
        tracing::info_span!(
            crate::tracing_otel::PROBE_SPAN,
            request_id = %request_id,
            correlation_id = %correlation_id,
            method = %request.method(),
            route = %route,
            trace_id = tracing::field::Empty,
        )
    } else {
        tracing::info_span!(
            "request",
            request_id = %request_id,
            correlation_id = %correlation_id,
            method = %request.method(),
            // The route rather than the path. A span leaves this process
            // for a backend that is not the record's custodian, and a path
            // carries a job id (SPECIFICATIONS.md sections 40 and 41). The
            // matched pattern does not exist yet here, since routing happens
            // further in, so the path is reduced to its route by the table
            // below.
            route = %route,
            trace_id = tracing::field::Empty,
        )
    };
    if let Some(id) = trace_id {
        span.record("trace_id", id.as_str());
    }
    set_parent(&span, parent);
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
    error::envelope(status, &code, &message, error::retryable_status(status))
}

/// Rejects a request that does not present the internal bearer token.
///
/// The token authenticates the gateway to the processor inside the cluster
/// (SPECIFICATIONS.md sections 30 and 31). It is not a user credential and
/// carries no identity: the gateway has already authenticated and authorised
/// the caller before it dispatches work, and this service performs no
/// user-level authorisation of its own.
///
/// When no token is configured the layer is not installed at all, so this
/// runs only where a credential is genuinely required. The comparison is
/// constant time, the token is never logged, and a failure says only that
/// authentication is required: which of "absent", "malformed" or "wrong" it
/// was is not something a caller needs, and telling them helps only an
/// attacker.
pub async fn authenticate(
    State(expected): State<Secret>,
    request: Request,
    next: Next,
) -> Response {
    let presented = request
        .headers()
        .get(header::AUTHORIZATION)
        .and_then(|value| value.to_str().ok())
        .and_then(|value| value.strip_prefix("Bearer "))
        .map(str::trim);

    if presented.is_some_and(|token| expected.matches(token)) {
        return next.run(request).await;
    }

    tracing::warn!(
        presented = presented.is_some(),
        "internal request rejected: authentication failed"
    );
    let mut response = Error::unauthenticated().into_response();
    // RFC 7235: a 401 states the scheme the client should use.
    response
        .headers_mut()
        .insert(header::WWW_AUTHENTICATE, HeaderValue::from_static("Bearer"));
    debug_assert_eq!(response.status(), StatusCode::UNAUTHORIZED);
    response
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
