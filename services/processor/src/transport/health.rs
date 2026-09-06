use axum::Json;
use axum::extract::State;
use axum::http::StatusCode;
use serde::Serialize;

use crate::state::SharedState;
use crate::{SERVICE_NAME, VERSION};

/// Body of `GET /health`.
#[derive(Debug, Clone, Serialize, PartialEq, Eq)]
pub struct HealthResponse {
    pub status: &'static str,
    pub service: &'static str,
    pub version: &'static str,
}

/// Body of `GET /ready`. `checks` lists dependency checks once there are
/// dependencies; failure causes never appear here.
#[derive(Debug, Clone, Serialize, PartialEq, Eq)]
pub struct ReadinessResponse {
    pub status: &'static str,
    pub service: &'static str,
    pub version: &'static str,
    pub checks: Vec<Check>,
}

/// One dependency check result.
#[derive(Debug, Clone, Serialize, PartialEq, Eq)]
pub struct Check {
    pub name: String,
    pub status: String,
}

/// Process liveness. Never consults dependencies.
pub async fn live() -> Json<HealthResponse> {
    Json(HealthResponse {
        status: "ok",
        service: SERVICE_NAME,
        version: VERSION,
    })
}

/// Readiness to accept work: 200 while serving, 503 before the listener is
/// up and from the moment shutdown begins.
pub async fn ready(State(state): State<SharedState>) -> (StatusCode, Json<ReadinessResponse>) {
    let ready = state.readiness.is_ready();
    let body = ReadinessResponse {
        status: if ready { "ready" } else { "not_ready" },
        service: SERVICE_NAME,
        version: VERSION,
        checks: Vec::new(),
    };
    let status = if ready {
        StatusCode::OK
    } else {
        StatusCode::SERVICE_UNAVAILABLE
    };
    (status, Json(body))
}
