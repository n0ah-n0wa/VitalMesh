use axum::Json;
use axum::extract::State;
use axum::http::StatusCode;
use serde::Serialize;

use crate::anomaly::ALGORITHM_VERSION;
use crate::domain::AlgorithmVersion;
use crate::state::SharedState;
use crate::{CONTRACT_VERSION, SERVICE_NAME, VERSION};

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

/// Body of `GET /internal/v1/health`, the contract's health response. It
/// carries the versions the gateway checks compatibility against and the
/// limits in force, so a client can refuse oversized work before sending it
/// and size its own timeouts above this processor's.
#[derive(Debug, Clone, Serialize, PartialEq, Eq)]
pub struct InternalHealthResponse {
    pub status: &'static str,
    pub service: &'static str,
    pub version: &'static str,
    pub algorithm_version: AlgorithmVersion,
    pub contract_version: &'static str,
    pub limits: Limits,
}

/// The bounds this processor applies, as the contract reports them.
#[derive(Debug, Clone, Serialize, PartialEq, Eq)]
pub struct Limits {
    pub max_job_measurements: u64,
    pub max_request_bytes: u64,
    pub processing_timeout_ms: u64,
    pub max_concurrent_jobs: u64,
    pub job_retention_seconds: u64,
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

/// The contract's health endpoint. It answers 200 whenever the process is
/// serving and consults no dependency, so it never blocks; readiness is the
/// unversioned `/ready`, which is outside the contract.
pub async fn internal(State(state): State<SharedState>) -> Json<InternalHealthResponse> {
    let processing = &state.config.processing;
    Json(InternalHealthResponse {
        status: "ok",
        service: SERVICE_NAME,
        version: VERSION,
        algorithm_version: ALGORITHM_VERSION,
        contract_version: CONTRACT_VERSION,
        limits: Limits {
            max_job_measurements: processing.max_job_measurements.get() as u64,
            max_request_bytes: state.config.http.max_body_bytes as u64,
            processing_timeout_ms: processing.timeout.as_millis() as u64,
            max_concurrent_jobs: processing.max_concurrent_jobs.get() as u64,
            job_retention_seconds: processing.job_retention.as_secs(),
        },
    })
}
