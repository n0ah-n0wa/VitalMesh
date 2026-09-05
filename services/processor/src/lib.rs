//! VitalMesh processor: HTTP router and health endpoints.

pub mod health;

use axum::{Router, routing::get};

/// Service name reported by the health endpoints.
pub const SERVICE_NAME: &str = "processor";

/// Build identifier, injected by the Makefile through `VITALMESH_VERSION`.
pub const VERSION: &str = match option_env!("VITALMESH_VERSION") {
    Some(version) => version,
    None => "dev",
};

/// Returns the HTTP router exposing the service's routes.
pub fn app() -> Router {
    Router::new()
        .route("/health", get(health::live))
        .route("/ready", get(health::ready))
}
