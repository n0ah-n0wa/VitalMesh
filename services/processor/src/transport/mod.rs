//! HTTP transport for the internal API: router assembly, middleware and
//! handlers. Every response is JSON; failures use the shared error envelope.

mod body;
pub mod context;
mod error;
mod health;
mod jobs;
pub mod middleware;
mod process;

#[cfg(test)]
mod tests;

use axum::Router;
use axum::extract::DefaultBodyLimit;
use axum::http::StatusCode;
use axum::response::Response;
use axum::routing::{get, post};

use crate::config::Http;
use crate::error::{Error, Kind};
use crate::state::SharedState;

pub use error::{ErrorBody, ErrorResponse};
pub use health::{Check, HealthResponse, InternalHealthResponse, Limits, ReadinessResponse};
pub use jobs::{JobError, JobStatusResponse};
pub use process::REQUEST_TIMEOUT_HEADER;

/// Builds the production router: the route table wrapped in the standard
/// middleware chain.
pub fn router(state: SharedState) -> Router {
    let http = state.config.http.clone();
    with_middleware(routes(&http), &http).with_state(state)
}

/// The route table without middleware or state.
///
/// The three `/internal/v1` routes are the contract
/// (`contracts/internal-api/processor-v1.json`). `/health` and `/ready` are
/// the unversioned platform probes and are deliberately outside it: they
/// serve Kubernetes, not the gateway, and may change without a contract
/// version bump.
pub fn routes(http: &Http) -> Router<SharedState> {
    let mut protected = Router::new()
        .route("/internal/v1/process", post(process::process))
        .route("/internal/v1/jobs/{job_id}", get(jobs::get));

    // The credential is required in staging and production; where none is
    // configured the layer is not installed rather than installed with an
    // empty token that would accept anything.
    if let Some(token) = http.internal_token.clone() {
        protected = protected.layer(axum::middleware::from_fn_with_state(
            token,
            middleware::authenticate,
        ));
    } else {
        tracing::warn!(
            "INTERNAL_TOKEN is not set: the internal API is served without authentication"
        );
    }

    Router::new()
        .route("/health", get(health::live))
        .route("/ready", get(health::ready))
        .route("/internal/v1/health", get(health::internal))
        .merge(protected)
}

/// Applies the standard middleware chain and fallbacks to `router`,
/// outermost first: request identification and logging, envelope mapping
/// for framework rejections, the request timeout, and the body size limit.
pub fn with_middleware(router: Router<SharedState>, http: &Http) -> Router<SharedState> {
    router
        .fallback(not_found)
        .method_not_allowed_fallback(method_not_allowed)
        .layer(DefaultBodyLimit::max(http.max_body_bytes))
        .layer(axum::middleware::from_fn_with_state(
            http.request_timeout,
            middleware::timeout,
        ))
        .layer(axum::middleware::from_fn(middleware::envelope_rejections))
        .layer(axum::middleware::from_fn(middleware::request_context))
}

async fn not_found() -> Error {
    Error::new(
        Kind::NotFound,
        "NOT_FOUND",
        "The requested resource does not exist.",
    )
}

async fn method_not_allowed() -> Response {
    error::envelope(
        StatusCode::METHOD_NOT_ALLOWED,
        "METHOD_NOT_ALLOWED",
        "The method is not allowed for this resource.",
        false,
    )
}
