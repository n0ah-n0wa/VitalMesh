//! Router tests driven through tower's `oneshot`, covering the middleware
//! chain, the health endpoints and the error envelope.

use std::num::NonZeroUsize;
use std::sync::Arc;
use std::time::Duration;

use axum::Router;
use axum::body::Body;
use axum::http::{Request, StatusCode, header};
use axum::response::Response;
use axum::routing::{get, post};
use tokio_util::sync::CancellationToken;
use tower::ServiceExt;

use super::{ErrorResponse, context, routes, with_middleware};
use crate::config::{Config, Environment, Http, Log, LogFormat, Processing, Tracing};
use crate::error::{Error, Kind};
use crate::requestid::{CORRELATION_ID_HEADER, REQUEST_ID_HEADER, is_valid};
use crate::state::{AppState, SharedState};

const BODY_LIMIT: usize = 64;

fn test_config() -> Config {
    Config {
        environment: Environment::Test,
        http: Http {
            addr: "127.0.0.1:0".parse().unwrap(),
            request_timeout: Duration::from_millis(100),
            max_body_bytes: BODY_LIMIT,
            internal_token: None,
        },
        log: Log {
            level: tracing::Level::INFO,
            format: LogFormat::Json,
        },
        tracing: Tracing {
            endpoint: String::new(),
            timeout: Duration::from_secs(10),
        },
        processing: Processing {
            max_concurrent_jobs: NonZeroUsize::MIN,
            max_batch_size: NonZeroUsize::MIN,
            max_job_measurements: NonZeroUsize::new(1000).unwrap(),
            max_measurement_age: Duration::from_secs(3600),
            max_future_skew: Duration::from_secs(60),
            timeout: Duration::from_secs(1),
            job_retention: Duration::from_secs(900),
            rules: crate::anomaly::RuleSet::empty(),
        },
        shutdown_timeout: Duration::from_secs(1),
    }
}

fn state() -> SharedState {
    Arc::new(AppState::new(test_config(), CancellationToken::new()))
}

/// The production routes plus test-only routes exercising the middleware.
fn app(state: SharedState) -> Router {
    let http = state.config.http.clone();
    let router = routes(&http)
        .route(
            "/slow",
            get(async || -> Result<&'static str, Error> {
                tokio::time::sleep(Duration::from_secs(5)).await;
                Ok("late")
            }),
        )
        .route(
            "/boom",
            get(async || -> Result<&'static str, Error> {
                Err(Error::internal(std::io::Error::other(
                    "db at 10.0.0.9:5432 refused",
                )))
            }),
        )
        .route(
            "/context",
            get(async || -> String {
                let ctx = context::current_context().expect("inside a request");
                format!("{}|{}", ctx.request_id, ctx.correlation_id)
            }),
        )
        .route(
            "/overloaded",
            get(async || -> Result<&'static str, Error> { Err(Error::overloaded()) }),
        )
        .route("/echo", post(async |body: String| body));
    with_middleware(router, &state.config.http).with_state(state)
}

async fn send(app: Router, request: Request<Body>) -> (Response, String) {
    let response = app.oneshot(request).await.unwrap();
    let (parts, body) = response.into_parts();
    let bytes = axum::body::to_bytes(body, 1 << 16).await.unwrap();
    (
        Response::from_parts(parts, Body::empty()),
        String::from_utf8(bytes.to_vec()).unwrap(),
    )
}

fn get_request(path: &str) -> Request<Body> {
    Request::get(path).body(Body::empty()).unwrap()
}

fn envelope(body: &str) -> ErrorResponse {
    serde_json::from_str(body).unwrap_or_else(|e| panic!("not an error envelope: {e}: {body}"))
}

#[tokio::test]
async fn health_reports_ok() {
    let (response, body) = send(app(state()), get_request("/health")).await;
    assert_eq!(response.status(), StatusCode::OK);
    let health: serde_json::Value = serde_json::from_str(&body).unwrap();
    assert_eq!(health["status"], "ok");
    assert_eq!(health["service"], "processor");
    assert!(health["version"].is_string());

    let (response, _) = send(app(state()), get_request("/internal/v1/health")).await;
    assert_eq!(response.status(), StatusCode::OK);
}

#[tokio::test]
async fn ready_follows_the_readiness_flag() {
    let state = state();
    let (response, body) = send(app(state.clone()), get_request("/ready")).await;
    assert_eq!(response.status(), StatusCode::SERVICE_UNAVAILABLE);
    assert!(body.contains("\"status\":\"not_ready\""));
    assert!(body.contains("\"checks\":[]"));

    state.readiness.set_ready(true);
    let (response, body) = send(app(state), get_request("/ready")).await;
    assert_eq!(response.status(), StatusCode::OK);
    assert!(body.contains("\"status\":\"ready\""));
}

#[tokio::test]
async fn request_and_correlation_ids_are_assigned_and_echoed() {
    let (response, body) = send(app(state()), get_request("/context")).await;
    let request_id = response.headers()[REQUEST_ID_HEADER].to_str().unwrap();
    let correlation_id = response.headers()[CORRELATION_ID_HEADER].to_str().unwrap();
    assert!(is_valid(request_id));
    assert_eq!(
        correlation_id, request_id,
        "correlation defaults to the request id"
    );
    assert_eq!(body, format!("{request_id}|{correlation_id}"));
}

#[tokio::test]
async fn valid_client_ids_are_kept_and_invalid_ones_replaced() {
    let request = Request::get("/context")
        .header(REQUEST_ID_HEADER, "gateway-req-7")
        .header(CORRELATION_ID_HEADER, "corr-42")
        .body(Body::empty())
        .unwrap();
    let (response, body) = send(app(state()), request).await;
    assert_eq!(response.headers()[REQUEST_ID_HEADER], "gateway-req-7");
    assert_eq!(response.headers()[CORRELATION_ID_HEADER], "corr-42");
    assert_eq!(body, "gateway-req-7|corr-42");

    let request = Request::get("/context")
        .header(REQUEST_ID_HEADER, "bad value!")
        .header(CORRELATION_ID_HEADER, "also bad;")
        .body(Body::empty())
        .unwrap();
    let (response, _) = send(app(state()), request).await;
    let request_id = response.headers()[REQUEST_ID_HEADER].to_str().unwrap();
    assert!(is_valid(request_id) && request_id != "bad value!");
    assert_eq!(
        response.headers()[CORRELATION_ID_HEADER].to_str().unwrap(),
        request_id
    );
}

#[tokio::test]
async fn unknown_route_and_wrong_method_use_the_envelope() {
    let (response, body) = send(app(state()), get_request("/internal/v1/nope")).await;
    assert_eq!(response.status(), StatusCode::NOT_FOUND);
    let env = envelope(&body);
    assert_eq!(env.error.code, "NOT_FOUND");
    assert_eq!(
        env.error.request_id,
        response.headers()[REQUEST_ID_HEADER].to_str().unwrap()
    );

    let request = Request::post("/health").body(Body::empty()).unwrap();
    let (response, body) = send(app(state()), request).await;
    assert_eq!(response.status(), StatusCode::METHOD_NOT_ALLOWED);
    let env = envelope(&body);
    assert_eq!(env.error.code, "METHOD_NOT_ALLOWED");
    assert!(!env.error.request_id.is_empty());
}

#[tokio::test]
async fn internal_errors_are_hidden() {
    let request = Request::get("/boom")
        .header(REQUEST_ID_HEADER, "trace-me")
        .body(Body::empty())
        .unwrap();
    let (response, body) = send(app(state()), request).await;
    assert_eq!(response.status(), StatusCode::INTERNAL_SERVER_ERROR);
    let env = envelope(&body);
    assert_eq!(env.error.code, "INTERNAL_ERROR");
    assert_eq!(env.error.request_id, "trace-me");
    assert!(!body.contains("10.0.0.9"), "cause leaked: {body}");
}

#[tokio::test]
async fn overloaded_gets_503_with_retry_after() {
    let (response, body) = send(app(state()), get_request("/overloaded")).await;
    assert_eq!(response.status(), StatusCode::SERVICE_UNAVAILABLE);
    assert_eq!(response.headers()[header::RETRY_AFTER], "1");
    assert_eq!(envelope(&body).error.code, "PROCESSOR_OVERLOADED");
}

#[tokio::test]
async fn slow_handlers_time_out() {
    let (response, body) = send(app(state()), get_request("/slow")).await;
    assert_eq!(response.status(), StatusCode::GATEWAY_TIMEOUT);
    assert_eq!(envelope(&body).error.code, "PROCESSING_TIMEOUT");
    assert!(!body.contains("late"));
}

#[tokio::test]
async fn oversized_bodies_are_rejected_with_the_envelope() {
    let request = Request::post("/echo")
        .body(Body::from("x".repeat(BODY_LIMIT + 1)))
        .unwrap();
    let (response, body) = send(app(state()), request).await;
    assert_eq!(response.status(), StatusCode::PAYLOAD_TOO_LARGE);
    assert_eq!(response.headers()[header::CONTENT_TYPE], "application/json");
    let env = envelope(&body);
    assert_eq!(env.error.code, "REQUEST_BODY_TOO_LARGE");
    assert_eq!(
        env.error.request_id,
        response.headers()[REQUEST_ID_HEADER].to_str().unwrap()
    );

    let request = Request::post("/echo")
        .body(Body::from("x".repeat(BODY_LIMIT)))
        .unwrap();
    let (response, body) = send(app(state()), request).await;
    assert_eq!(response.status(), StatusCode::OK);
    assert_eq!(body.len(), BODY_LIMIT);
}

#[test]
fn status_mapping_covers_every_kind() {
    for kind in [
        Kind::Internal,
        Kind::Invalid,
        Kind::Validation,
        Kind::NotFound,
        Kind::Conflict,
        Kind::Overloaded,
        Kind::Timeout,
        Kind::Cancelled,
        Kind::Unavailable,
    ] {
        let status = super::error::status_for(kind);
        assert!(
            status.is_client_error() || status.is_server_error(),
            "{kind:?} -> {status}"
        );
    }
    assert_eq!(
        super::error::code_for_status(StatusCode::IM_A_TEAPOT),
        "I'M_A_TEAPOT"
    );
}
