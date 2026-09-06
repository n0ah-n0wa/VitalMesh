//! HTTP transport for the internal API: router assembly, middleware and
//! handlers. Every response is JSON; failures use the shared error envelope.

pub mod context;
mod error;
mod health;
pub mod middleware;

#[cfg(test)]
mod tests;

use axum::Router;
use axum::extract::DefaultBodyLimit;
use axum::http::StatusCode;
use axum::response::Response;
use axum::routing::get;

use crate::config::Http;
use crate::error::{Error, Kind};
use crate::state::SharedState;

pub use error::{ErrorBody, ErrorResponse};
pub use health::{Check, HealthResponse, ReadinessResponse};

/// Builds the production router: the route table wrapped in the standard
/// middleware chain.
pub fn router(state: SharedState) -> Router {
    with_middleware(routes(), &state.config.http).with_state(state)
}

/// The route table without middleware or state.
pub fn routes() -> Router<SharedState> {
    Router::new()
        .route("/health", get(health::live))
        .route("/ready", get(health::ready))
        .route("/internal/v1/health", get(health::live))
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
    )
}
