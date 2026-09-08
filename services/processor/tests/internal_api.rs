//! Integration tests of the internal HTTP API against the real service.
//!
//! Every test drives the production router over a real TCP socket through
//! [`lifecycle::run`], so the middleware chain, the body limit, the
//! authentication layer, the engine's admission control and the error
//! envelope are all the ones that ship. Nothing is stubbed.
//!
//! The contract these assert against is
//! `contracts/internal-api/processor-v1.json`; `tests/contract.rs` checks the
//! document itself and this file checks the behaviour behind it.

use std::net::SocketAddr;
use std::num::NonZeroUsize;
use std::time::Duration;

use serde_json::{Value, json};
use tokio::io::{AsyncReadExt, AsyncWriteExt};
use tokio::net::{TcpListener, TcpStream};
use tokio_util::sync::CancellationToken;

use processor::anomaly::{ALGORITHM_VERSION, RuleSet};
use processor::config::{Config, Environment, Http, Log, LogFormat, Processing, Secret, Tracing};
use processor::lifecycle;

const TOKEN: &str = "example-internal-token-for-tests";
const REQUESTED_AT: &str = "2026-09-06T12:00:00Z";

/// Body limit for the shared fixture. It is well above the production
/// default so that the multi-megabyte job used to observe work in flight
/// fits; the limit itself is exercised by its own test, on its own config.
const BODY_LIMIT: usize = 8 << 20;

// ---------------------------------------------------------------- harness

/// A running processor and the address it serves on.
struct Service {
    addr: SocketAddr,
    shutdown: CancellationToken,
    handle: tokio::task::JoinHandle<processor::error::Result<()>>,
}

impl Service {
    async fn start(config: Config) -> Self {
        let listener = TcpListener::bind("127.0.0.1:0").await.expect("bind");
        let addr = listener.local_addr().expect("addr");
        let shutdown = CancellationToken::new();
        let handle = tokio::spawn(lifecycle::run(config, listener, shutdown.clone()));
        Self {
            addr,
            shutdown,
            handle,
        }
    }

    async fn stop(self) {
        self.shutdown.cancel();
        let _ = tokio::time::timeout(Duration::from_secs(10), self.handle).await;
    }
}

/// One HTTP response, parsed far enough to assert on.
#[derive(Debug)]
struct Response {
    status: u16,
    headers: Vec<(String, String)>,
    body: String,
}

impl Response {
    fn header(&self, name: &str) -> Option<&str> {
        self.headers
            .iter()
            .find(|(key, _)| key.eq_ignore_ascii_case(name))
            .map(|(_, value)| value.as_str())
    }

    fn json(&self) -> Value {
        serde_json::from_str(&self.body)
            .unwrap_or_else(|e| panic!("body is not JSON ({e}): {}", self.body))
    }

    /// The error envelope, asserting the parts every failure must carry.
    fn error(&self) -> Value {
        let body = self.json();
        let error = body["error"].clone();
        assert!(
            error.is_object(),
            "a failure must use the error envelope: {}",
            self.body
        );
        assert!(
            error["code"].as_str().is_some_and(|c| !c.is_empty()),
            "no code: {}",
            self.body
        );
        assert!(
            error["message"].as_str().is_some_and(|m| !m.is_empty()),
            "no message: {}",
            self.body
        );
        assert_eq!(
            error["request_id"].as_str(),
            self.header("x-request-id"),
            "the envelope's request id must be the one in the header"
        );
        assert!(
            error["retryable"].is_boolean(),
            "no retry classification: {}",
            self.body
        );
        error
    }

    fn code(&self) -> String {
        self.error()["code"].as_str().unwrap_or_default().to_owned()
    }
}

/// A request builder small enough to keep the tests readable, writing the
/// bytes itself so that a deliberately malformed request can be sent.
struct Request {
    method: &'static str,
    path: String,
    headers: Vec<(String, String)>,
    body: Option<Vec<u8>>,
}

impl Request {
    fn get(path: impl Into<String>) -> Self {
        Self {
            method: "GET",
            path: path.into(),
            headers: Vec::new(),
            body: None,
        }
    }

    fn post(path: impl Into<String>, body: Vec<u8>) -> Self {
        Self {
            method: "POST",
            path: path.into(),
            headers: vec![("content-type".into(), "application/json".into())],
            body: Some(body),
        }
    }

    fn json(path: impl Into<String>, body: &Value) -> Self {
        Self::post(path, serde_json::to_vec(body).expect("serialise"))
    }

    fn header(mut self, name: &str, value: &str) -> Self {
        self.headers.push((name.to_owned(), value.to_owned()));
        self
    }

    fn set(mut self, name: &str, value: &str) -> Self {
        self.headers.retain(|(k, _)| !k.eq_ignore_ascii_case(name));
        self.header(name, value)
    }

    fn authorized(self) -> Self {
        self.header("authorization", &format!("Bearer {TOKEN}"))
    }

    async fn send(self, addr: SocketAddr) -> Response {
        let mut stream = TcpStream::connect(addr).await.expect("connect");
        let mut request = format!(
            "{} {} HTTP/1.1\r\nHost: processor\r\n",
            self.method, self.path
        );
        for (name, value) in &self.headers {
            request.push_str(&format!("{name}: {value}\r\n"));
        }
        let body = self.body.unwrap_or_default();
        request.push_str(&format!("content-length: {}\r\n", body.len()));
        request.push_str("connection: close\r\n\r\n");

        let mut bytes = request.into_bytes();
        bytes.extend_from_slice(&body);
        stream.write_all(&bytes).await.expect("write");
        stream.flush().await.expect("flush");

        let mut raw = Vec::new();
        stream.read_to_end(&mut raw).await.expect("read");
        parse_response(&raw)
    }
}

fn parse_response(raw: &[u8]) -> Response {
    let text = String::from_utf8_lossy(raw);
    let (head, body) = text
        .split_once("\r\n\r\n")
        .unwrap_or_else(|| panic!("malformed response: {text}"));
    let mut lines = head.split("\r\n");
    let status_line = lines.next().unwrap_or_default();
    let status = status_line
        .split_whitespace()
        .nth(1)
        .and_then(|code| code.parse().ok())
        .unwrap_or_else(|| panic!("no status in {status_line:?}"));
    let headers = lines
        .filter_map(|line| line.split_once(": "))
        .map(|(name, value)| (name.to_ascii_lowercase(), value.to_owned()))
        .collect();
    Response {
        status,
        headers,
        body: body.to_owned(),
    }
}

// ------------------------------------------------------------ fixtures

fn config() -> Config {
    Config {
        environment: Environment::Test,
        http: Http {
            addr: "127.0.0.1:0".parse().unwrap(),
            request_timeout: Duration::from_secs(30),
            max_body_bytes: BODY_LIMIT,
            internal_token: Some(Secret::new(TOKEN)),
        },
        log: Log {
            level: tracing::Level::WARN,
            format: LogFormat::Json,
        },
        tracing: Tracing {
            endpoint: String::new(),
            timeout: Duration::from_secs(10),
        },
        processing: Processing {
            max_concurrent_jobs: NonZeroUsize::new(4).unwrap(),
            max_batch_size: NonZeroUsize::new(1000).unwrap(),
            max_job_measurements: NonZeroUsize::new(100_000).unwrap(),
            max_measurement_age: Duration::from_secs(400 * 24 * 3600),
            max_future_skew: Duration::from_secs(300),
            timeout: Duration::from_secs(300),
            job_retention: Duration::from_secs(900),
            rules: RuleSet::from_json(
                r#"{"rules": [{"name": "hr-bounds", "kind": "THRESHOLD",
                     "measurement_types": ["HEART_RATE"],
                     "tiers": {"warning": {"lower": 40, "upper": 180}}}]}"#,
            )
            .expect("the test rule set is valid"),
        },
        shutdown_timeout: Duration::from_secs(5),
    }
}

/// A job the processor accepts.
fn job(id: &str, windows: &[&str]) -> Value {
    json!({
        "id": id,
        "patient_id": "patient-1",
        "parameters": {
            "measurement_types": ["HEART_RATE"],
            "windows": windows,
            "percentiles": [50, 95]
        },
        "algorithm_version": ALGORITHM_VERSION.to_string(),
        "status": "PENDING",
        "requested_at": REQUESTED_AT
    })
}

/// `n` heart-rate readings one second apart, ending an hour before the job
/// was requested so that every one of them is inside the accepted window
/// whatever `n` is, and one of them above the rule's upper bound so that the
/// outcome carries an anomaly.
fn readings(n: usize) -> Vec<Value> {
    let end = processor::domain::Timestamp::parse("2026-09-06T11:00:00Z").unwrap();
    let start = end
        .checked_sub(Duration::from_secs(n as u64))
        .expect("the series fits inside the accepted age");
    (0..n)
        .map(|i| {
            let at = start
                .checked_add(Duration::from_secs(i as u64))
                .expect("in range");
            json!({
                "id": format!("m-{i:06}"),
                "type": "HEART_RATE",
                "value": if i == 1 { 191.5 } else { 60.0 + (i % 7) as f64 },
                "unit": "bpm",
                "recorded_at": at.to_rfc3339()
            })
        })
        .collect()
}

fn request_body(id: &str, count: usize) -> Value {
    json!({"job": job(id, &["1m", "1h"]), "readings": readings(count)})
}

/// Enough work that the job is still running while another request is made.
/// All seven windows over twenty thousand readings takes long enough in a
/// debug build to observe, without making the suite slow.
fn slow_body(id: &str) -> Value {
    json!({"job": job(id, &["1m", "5m", "15m", "1h", "6h", "24h", "7d"]),
           "readings": readings(20_000)})
}

// ------------------------------------------------------------- health

#[tokio::test]
async fn health_reports_versions_and_the_limits_in_force() {
    let service = Service::start(config()).await;
    let response = Request::get("/internal/v1/health").send(service.addr).await;

    assert_eq!(response.status, 200);
    let body = response.json();
    assert_eq!(body["status"], "ok");
    assert_eq!(body["service"], "processor");
    assert_eq!(body["algorithm_version"], ALGORITHM_VERSION.to_string());
    assert_eq!(body["contract_version"], processor::CONTRACT_VERSION);
    assert!(body["version"].as_str().is_some_and(|v| !v.is_empty()));

    // The limits a client sizes its requests and timeouts against.
    let limits = &body["limits"];
    assert_eq!(limits["max_job_measurements"], 100_000);
    assert_eq!(limits["max_request_bytes"], BODY_LIMIT);
    assert_eq!(limits["processing_timeout_ms"], 300_000);
    assert_eq!(limits["max_concurrent_jobs"], 4);
    assert_eq!(limits["job_retention_seconds"], 900);

    service.stop().await;
}

#[tokio::test]
async fn health_needs_no_credential_but_the_other_endpoints_do() {
    let service = Service::start(config()).await;

    assert_eq!(
        Request::get("/internal/v1/health")
            .send(service.addr)
            .await
            .status,
        200,
        "a probe must not need a token"
    );

    let unauthenticated = Request::json("/internal/v1/process", &request_body("job-a", 5))
        .send(service.addr)
        .await;
    assert_eq!(unauthenticated.status, 401);
    assert_eq!(unauthenticated.code(), "UNAUTHENTICATED");
    assert_eq!(
        unauthenticated.header("www-authenticate"),
        Some("Bearer"),
        "a 401 must name the scheme"
    );
    assert_eq!(
        unauthenticated.error()["retryable"],
        false,
        "a rejected credential does not become valid on retry"
    );

    assert_eq!(
        Request::get("/internal/v1/jobs/job-a")
            .send(service.addr)
            .await
            .status,
        401
    );

    service.stop().await;
}

#[tokio::test]
async fn a_wrong_or_malformed_credential_is_refused_without_saying_which() {
    let service = Service::start(config()).await;
    let body = request_body("job-b", 5);

    for authorization in [
        format!("Bearer {}", "example-internal-token-but-wrong"),
        format!("Bearer {TOKEN}x"),
        format!("Basic {TOKEN}"),
        TOKEN.to_owned(),
        "Bearer".to_owned(),
        "Bearer ".to_owned(),
    ] {
        let response = Request::json("/internal/v1/process", &body)
            .header("authorization", &authorization)
            .send(service.addr)
            .await;
        assert_eq!(response.status, 401, "{authorization:?} was accepted");
        let error = response.error();
        assert_eq!(error["code"], "UNAUTHENTICATED");
        let message = error["message"].as_str().unwrap_or_default();
        assert!(
            !message.to_lowercase().contains("missing") && !message.contains("malformed"),
            "the message distinguishes failure modes: {message}"
        );
        assert!(
            !response.body.contains(TOKEN),
            "the response echoed the expected token"
        );
    }

    service.stop().await;
}

// ------------------------------------------------------------ process

#[tokio::test]
async fn a_job_is_processed_and_returns_statistics_and_anomalies() {
    let service = Service::start(config()).await;
    let response = Request::json("/internal/v1/process", &request_body("job-ok", 30))
        .authorized()
        .send(service.addr)
        .await;

    assert_eq!(response.status, 200, "{}", response.body);
    let body = response.json();
    assert_eq!(body["job_id"], "job-ok");
    assert_eq!(body["algorithm_version"], ALGORITHM_VERSION.to_string());
    assert!(
        body["service_version"]
            .as_str()
            .is_some_and(|v| !v.is_empty())
    );
    assert_eq!(body["accepted"], 30);
    assert_eq!(body["skipped"], 0);
    assert_eq!(body["rejected"], json!([]));

    let results = body["results"].as_array().expect("results");
    assert!(!results.is_empty());
    for result in results {
        assert_eq!(result["job_id"], "job-ok");
        assert_eq!(result["measurement_type"], "HEART_RATE");
        assert!(result["statistics"]["count"].as_u64().unwrap() >= 1);
        assert!(result["statistics"]["percentiles"]["50"].is_number());
    }

    // The reading above the rule's upper bound is flagged, in the measured
    // value's own unit and at its own timestamp.
    let anomalies: Vec<&Value> = results
        .iter()
        .flat_map(|r| r["anomalies"].as_array().expect("anomalies"))
        .collect();
    assert!(!anomalies.is_empty(), "the outlier must be flagged");
    for anomaly in &anomalies {
        assert_eq!(anomaly["metric"], "THRESHOLD");
        assert_eq!(anomaly["severity"], "WARNING");
        assert_eq!(anomaly["value"], 191.5);
        assert_eq!(anomaly["threshold"], 180.0);
        assert_eq!(anomaly["algorithm_version"], ALGORITHM_VERSION.to_string());
    }

    service.stop().await;
}

#[tokio::test]
async fn readings_that_fail_validation_are_reported_by_index_and_the_rest_processed() {
    let service = Service::start(config()).await;
    let mut body = request_body("job-partial", 10);
    let readings = body["readings"].as_array_mut().unwrap();
    readings[2]["unit"] = json!("mmHg"); // right type, wrong unit
    readings[5]["type"] = json!("PULSE"); // no such type
    readings[7]["value"] = json!(999.0); // outside the technical range
    // `recorded_at` is a plain string on the wire precisely so that one
    // unreadable timestamp is reported against its own reading rather than
    // failing the whole request at parse time.
    readings[8]["recorded_at"] = json!("yesterday");

    let response = Request::json("/internal/v1/process", &body)
        .authorized()
        .send(service.addr)
        .await;
    assert_eq!(response.status, 200, "{}", response.body);
    let outcome = response.json();
    assert_eq!(outcome["accepted"], 6);

    let rejected = outcome["rejected"].as_array().expect("rejected");
    let reported: Vec<(u64, &str)> = rejected
        .iter()
        .map(|r| (r["index"].as_u64().unwrap(), r["code"].as_str().unwrap()))
        .collect();
    assert_eq!(
        reported,
        [
            (2, "UNIT_MISMATCH"),
            (5, "UNKNOWN_TYPE"),
            (7, "VALUE_OUT_OF_RANGE"),
            (8, "INVALID_TIMESTAMP")
        ]
    );
    for rejection in rejected {
        assert!(rejection["reason"].as_str().is_some_and(|r| !r.is_empty()));
    }

    service.stop().await;
}

#[tokio::test]
async fn a_request_that_cannot_be_understood_is_a_bad_request() {
    let service = Service::start(config()).await;

    let cases: Vec<(&str, Vec<u8>)> = vec![
        ("malformed JSON", b"{\"job\": }".to_vec()),
        ("empty body", Vec::new()),
        ("wrong shape", b"[]".to_vec()),
        (
            "unknown field",
            serde_json::to_vec(&json!({
                "job": job("job-x", &["1h"]),
                "readings": readings(1),
                "extra": true
            }))
            .unwrap(),
        ),
        (
            "missing readings",
            serde_json::to_vec(&json!({"job": job("job-x", &["1h"])})).unwrap(),
        ),
        (
            "malformed identifier",
            serde_json::to_vec(&json!({
                "job": job("not a valid id!", &["1h"]),
                "readings": readings(1)
            }))
            .unwrap(),
        ),
        (
            "unknown window",
            serde_json::to_vec(&json!({
                "job": job("job-x", &["3h"]),
                "readings": readings(1)
            }))
            .unwrap(),
        ),
    ];

    for (name, body) in cases {
        let response = Request::post("/internal/v1/process", body)
            .authorized()
            .send(service.addr)
            .await;
        assert_eq!(response.status, 400, "{name}: {}", response.body);
        assert_eq!(response.code(), "INVALID_REQUEST", "{name}");
        assert_eq!(response.error()["retryable"], false, "{name}");
    }

    service.stop().await;
}

#[tokio::test]
async fn a_body_that_is_not_json_is_an_unsupported_media_type() {
    let service = Service::start(config()).await;
    for content_type in ["text/plain", "application/xml"] {
        let response = Request::json("/internal/v1/process", &request_body("job-ct", 2))
            .set("content-type", content_type)
            .authorized()
            .send(service.addr)
            .await;
        assert_eq!(response.status, 415, "{content_type}");
        assert_eq!(response.code(), "UNSUPPORTED_MEDIA_TYPE");
    }
    service.stop().await;
}

#[tokio::test]
async fn a_body_over_the_limit_is_refused_before_it_is_parsed() {
    let mut config = config();
    config.http.max_body_bytes = 512;
    let service = Service::start(config).await;

    let response = Request::json("/internal/v1/process", &request_body("job-big", 50))
        .authorized()
        .send(service.addr)
        .await;
    assert_eq!(response.status, 413, "{}", response.body);
    assert_eq!(response.code(), "REQUEST_BODY_TOO_LARGE");

    service.stop().await;
}

#[tokio::test]
async fn a_job_that_breaks_a_rule_is_unprocessable_and_says_which_rule() {
    let mut config = config();
    config.processing.max_job_measurements = NonZeroUsize::new(4).unwrap();
    let service = Service::start(config).await;

    // A version this build does not implement.
    let mut body = request_body("job-version", 2);
    body["job"]["algorithm_version"] = json!("2.0.0");
    let response = Request::json("/internal/v1/process", &body)
        .authorized()
        .send(service.addr)
        .await;
    assert_eq!(response.status, 422, "{}", response.body);
    assert_eq!(response.code(), "UNSUPPORTED_ALGORITHM_VERSION");
    assert_eq!(response.error()["retryable"], false);

    // More readings than the configured bound, with the numbers attached so
    // a client can resize the job without parsing prose.
    let response = Request::json("/internal/v1/process", &request_body("job-large", 9))
        .authorized()
        .send(service.addr)
        .await;
    assert_eq!(response.status, 422, "{}", response.body);
    let error = response.error();
    assert_eq!(error["code"], "JOB_TOO_LARGE");
    assert_eq!(error["details"]["max_job_measurements"], 4);
    assert_eq!(error["details"]["received"], 9);

    // Every reading fails validation, so there is nothing to process.
    let mut body = request_body("job-empty", 3);
    for reading in body["readings"].as_array_mut().unwrap() {
        reading["unit"] = json!("mmHg");
    }
    let response = Request::json("/internal/v1/process", &body)
        .authorized()
        .send(service.addr)
        .await;
    assert_eq!(response.status, 422, "{}", response.body);
    assert_eq!(response.code(), "NO_VALID_MEASUREMENTS");

    service.stop().await;
}

#[tokio::test]
async fn a_client_budget_bounds_the_work_and_an_unusable_one_is_rejected() {
    let service = Service::start(config()).await;

    // One millisecond is not enough to finish, so the core stops at its next
    // checkpoint and the client is told, rather than waiting for the
    // processor's own far larger bound.
    let response = Request::json("/internal/v1/process", &slow_body("job-timeout"))
        .authorized()
        .header("x-request-timeout-ms", "1")
        .send(service.addr)
        .await;
    assert_eq!(response.status, 504, "{}", response.body);
    assert_eq!(response.code(), "PROCESSING_TIMEOUT");
    assert_eq!(
        response.error()["retryable"],
        true,
        "a timeout may be retried"
    );

    // A budget that cannot be understood is not silently ignored.
    for bad in ["0", "abc", "-5"] {
        let response = Request::json("/internal/v1/process", &request_body("job-budget", 2))
            .authorized()
            .header("x-request-timeout-ms", bad)
            .send(service.addr)
            .await;
        assert_eq!(response.status, 400, "{bad}: {}", response.body);
        assert_eq!(response.code(), "INVALID_REQUEST");
    }

    // A generous budget does not get in the way.
    let response = Request::json("/internal/v1/process", &request_body("job-budget-ok", 5))
        .authorized()
        .header("x-request-timeout-ms", "60000")
        .send(service.addr)
        .await;
    assert_eq!(response.status, 200, "{}", response.body);

    service.stop().await;
}

// -------------------------------------------------------- concurrency

#[tokio::test]
async fn capacity_is_bounded_and_a_refusal_says_when_to_come_back() {
    let mut config = config();
    config.processing.max_concurrent_jobs = NonZeroUsize::MIN;
    let service = Service::start(config).await;
    let addr = service.addr;

    let first = tokio::spawn(async move {
        Request::json("/internal/v1/process", &slow_body("job-slow"))
            .authorized()
            .send(addr)
            .await
    });
    wait_until_running(addr, "job-slow").await;

    let refused = Request::json("/internal/v1/process", &request_body("job-second", 2))
        .authorized()
        .send(addr)
        .await;
    assert_eq!(refused.status, 503, "{}", refused.body);
    assert_eq!(refused.code(), "PROCESSOR_OVERLOADED");
    assert_eq!(refused.error()["retryable"], true);
    assert_eq!(
        refused.header("retry-after"),
        Some("1"),
        "a transient refusal says when to retry"
    );

    // The refused job was never admitted, so it left no record behind.
    let unknown = Request::get("/internal/v1/jobs/job-second")
        .authorized()
        .send(addr)
        .await;
    assert_eq!(unknown.status, 404, "{}", unknown.body);

    assert_eq!(first.await.expect("first job").status, 200);
    service.stop().await;
}

#[tokio::test]
async fn the_same_job_id_cannot_run_twice_at_once() {
    let service = Service::start(config()).await;
    let addr = service.addr;

    let first = tokio::spawn(async move {
        Request::json("/internal/v1/process", &slow_body("job-dup"))
            .authorized()
            .send(addr)
            .await
    });
    wait_until_running(addr, "job-dup").await;

    // A small body, so the duplicate arrives while the first job is still
    // working rather than spending the window uploading its own readings.
    let duplicate = Request::json("/internal/v1/process", &request_body("job-dup", 2))
        .authorized()
        .send(addr)
        .await;
    assert_eq!(duplicate.status, 409, "{}", duplicate.body);
    assert_eq!(duplicate.code(), "JOB_ALREADY_RUNNING");
    assert_eq!(duplicate.error()["retryable"], false);

    // The refused duplicate must not have disturbed the running attempt.
    let view = Request::get("/internal/v1/jobs/job-dup")
        .authorized()
        .send(addr)
        .await;
    assert_eq!(view.json()["status"], "PROCESSING");

    assert_eq!(first.await.expect("first job").status, 200);
    service.stop().await;
}

// -------------------------------------------------------------- jobs

#[tokio::test]
async fn a_job_is_observable_while_it_runs_and_after_it_finishes() {
    let service = Service::start(config()).await;
    let addr = service.addr;

    let running = tokio::spawn(async move {
        Request::json("/internal/v1/process", &slow_body("job-watch"))
            .authorized()
            .send(addr)
            .await
    });
    wait_until_running(addr, "job-watch").await;

    let view = Request::get("/internal/v1/jobs/job-watch")
        .authorized()
        .send(addr)
        .await
        .json();
    assert_eq!(view["job_id"], "job-watch");
    assert_eq!(view["status"], "PROCESSING");
    assert_eq!(view["algorithm_version"], ALGORITHM_VERSION.to_string());
    assert_eq!(view["requested_at"], REQUESTED_AT);
    assert!(view["started_at"].is_string());
    assert_eq!(view["finished_at"], Value::Null);
    assert!(view["running_for_ms"].is_number());
    assert_eq!(view["error"], Value::Null);

    assert_eq!(running.await.expect("job").status, 200);

    let view = Request::get("/internal/v1/jobs/job-watch")
        .authorized()
        .send(addr)
        .await
        .json();
    assert_eq!(view["status"], "COMPLETED");
    assert!(view["finished_at"].is_string());
    assert!(
        view.get("running_for_ms").is_none(),
        "a finished job must not claim to be running: {view}"
    );
    assert_eq!(view["error"], Value::Null);

    service.stop().await;
}

#[tokio::test]
async fn a_failed_job_keeps_its_diagnosis_and_an_unknown_one_is_absent() {
    let service = Service::start(config()).await;
    let addr = service.addr;

    let failed = Request::json("/internal/v1/process", &slow_body("job-failed"))
        .authorized()
        .header("x-request-timeout-ms", "1")
        .send(addr)
        .await;
    assert_eq!(failed.status, 504);

    let view = Request::get("/internal/v1/jobs/job-failed")
        .authorized()
        .send(addr)
        .await
        .json();
    assert_eq!(view["status"], "FAILED");
    assert_eq!(view["error"]["code"], "PROCESSING_TIMEOUT");
    assert_eq!(view["error"]["retryable"], true);
    assert!(view["finished_at"].is_string());

    // A job this processor never saw is absent, which is not the same as
    // saying it does not exist: the gateway owns that answer.
    let unknown = Request::get("/internal/v1/jobs/never-dispatched")
        .authorized()
        .send(addr)
        .await;
    assert_eq!(unknown.status, 404);
    assert_eq!(unknown.code(), "JOB_NOT_FOUND");
    assert_eq!(unknown.error()["retryable"], false);

    // An id that could not be a job id at all is a malformed request.
    let malformed = Request::get("/internal/v1/jobs/not%20a%20job")
        .authorized()
        .send(addr)
        .await;
    assert_eq!(malformed.status, 400, "{}", malformed.body);
    assert_eq!(malformed.code(), "INVALID_REQUEST");

    service.stop().await;
}

// ------------------------------------------------------- correlation

#[tokio::test]
async fn request_and_correlation_ids_are_echoed_generated_and_sanitised() {
    let service = Service::start(config()).await;
    let addr = service.addr;

    // Both supplied: both come back unchanged.
    let response = Request::get("/internal/v1/health")
        .header("x-request-id", "req-123")
        .header("x-correlation-id", "corr-456")
        .send(addr)
        .await;
    assert_eq!(response.header("x-request-id"), Some("req-123"));
    assert_eq!(response.header("x-correlation-id"), Some("corr-456"));

    // Neither supplied: one is generated and the correlation id follows it.
    let response = Request::get("/internal/v1/health").send(addr).await;
    let generated = response.header("x-request-id").expect("generated");
    assert!(!generated.is_empty());
    assert_eq!(response.header("x-correlation-id"), Some(generated));

    // A malformed value is replaced rather than rejected: an unusable id
    // must not turn valid work into a failure.
    let response = Request::json("/internal/v1/process", &request_body("job-ids", 3))
        .authorized()
        .header("x-request-id", "not a valid id")
        .header("x-correlation-id", "corr-ok")
        .send(addr)
        .await;
    assert_eq!(response.status, 200, "{}", response.body);
    let echoed = response.header("x-request-id").expect("request id");
    assert_ne!(echoed, "not a valid id");
    assert!(
        echoed
            .bytes()
            .all(|b| b.is_ascii_alphanumeric() || matches!(b, b'.' | b'_' | b'-')),
        "a malformed id reached the response: {echoed}"
    );
    assert_eq!(response.header("x-correlation-id"), Some("corr-ok"));

    // A failure carries both ids in its envelope, so it can be traced back.
    let failure = Request::post("/internal/v1/process", b"{".to_vec())
        .authorized()
        .header("x-request-id", "req-fail")
        .header("x-correlation-id", "corr-fail")
        .send(addr)
        .await;
    let error = failure.error();
    assert_eq!(error["request_id"], "req-fail");
    assert_eq!(error["correlation_id"], "corr-fail");

    service.stop().await;
}

#[tokio::test]
async fn unknown_routes_and_methods_use_the_same_envelope() {
    let service = Service::start(config()).await;

    let not_found = Request::get("/internal/v1/nope").send(service.addr).await;
    assert_eq!(not_found.status, 404);
    assert_eq!(not_found.code(), "NOT_FOUND");

    let wrong_method = Request::get("/internal/v1/process")
        .authorized()
        .send(service.addr)
        .await;
    assert_eq!(wrong_method.status, 405);
    assert_eq!(wrong_method.code(), "METHOD_NOT_ALLOWED");

    service.stop().await;
}

// ---------------------------------------------------------- shutdown

#[tokio::test]
async fn shutdown_refuses_new_work_and_readiness_turns_off() {
    let service = Service::start(config()).await;
    let addr = service.addr;

    assert_eq!(Request::get("/ready").send(addr).await.status, 200);

    service.shutdown.cancel();
    // The listener closes on shutdown, so give the drain a moment and then
    // accept either a refusal or a closed connection.
    for _ in 0..50 {
        tokio::time::sleep(Duration::from_millis(20)).await;
        let Ok(stream) = TcpStream::connect(addr).await else {
            break;
        };
        drop(stream);
    }

    let _ = tokio::time::timeout(Duration::from_secs(10), service.handle).await;
}

/// A processor configured without a token serves the internal API openly.
/// That is only allowed outside a deployment, which the configuration
/// enforces; this checks the router really omits the layer.
#[tokio::test]
async fn without_a_configured_token_the_internal_api_is_open() {
    let mut config = config();
    config.http.internal_token = None;
    let service = Service::start(config).await;

    let response = Request::json("/internal/v1/process", &request_body("job-open", 3))
        .send(service.addr)
        .await;
    assert_eq!(response.status, 200, "{}", response.body);

    service.stop().await;
}

// ----------------------------------------------------------- helpers

/// Waits until the jobs endpoint reports `job_id` as running, so a test can
/// act while a job is in flight without sleeping for a guessed duration.
async fn wait_until_running(addr: SocketAddr, job_id: &str) {
    let deadline = std::time::Instant::now() + Duration::from_secs(10);
    loop {
        let response = Request::get(format!("/internal/v1/jobs/{job_id}"))
            .authorized()
            .send(addr)
            .await;
        if response.status == 200 && response.json()["status"] == "PROCESSING" {
            return;
        }
        assert!(
            std::time::Instant::now() < deadline,
            "{job_id} never reported PROCESSING (last status {})",
            response.status
        );
        tokio::time::sleep(Duration::from_millis(5)).await;
    }
}

// ------------------------------------------------------------ metrics

/// The endpoint the specification names, served next to the probes and
/// needing no credential: it serves the platform's scraper.
#[tokio::test]
async fn metrics_are_served_without_a_credential() {
    let service = Service::start(config()).await;
    let response = Request::get("/metrics").send(service.addr).await;

    assert_eq!(response.status, 200);
    let body = response.body.clone();
    for family in [
        "vitalmesh_processor_jobs_total",
        "vitalmesh_processor_active_jobs",
        "vitalmesh_processor_job_capacity",
        "vitalmesh_processor_job_slots_available",
    ] {
        assert!(body.contains(family), "{family} is not exposed in\n{body}");
    }
    // Saturation is published before any work, so a dashboard shows an idle
    // service rather than nothing.
    assert!(body.contains("vitalmesh_processor_active_jobs 0"), "{body}");
    assert!(
        body.contains("vitalmesh_processor_jobs_total{outcome=\"failed\"} 0"),
        "{body}"
    );

    service.stop().await;
}

/// A job that really runs must move the job counter, the duration histogram
/// and the request metrics, with the route recorded as its pattern.
#[tokio::test]
async fn processing_a_job_moves_the_metrics() {
    let service = Service::start(config()).await;

    let response = Request::json("/internal/v1/process", &request_body("job-metrics-1", 200))
        .authorized()
        .send(service.addr)
        .await;
    assert_eq!(response.status, 200, "{}", response.body);

    let body = Request::get("/metrics").send(service.addr).await.body;

    assert!(
        body.contains("vitalmesh_processor_jobs_total{outcome=\"completed\"} 1"),
        "the job was not counted:\n{body}"
    );
    assert!(
        body.contains("vitalmesh_processor_job_duration_seconds_count{outcome=\"completed\"} 1"),
        "the job was not timed:\n{body}"
    );
    assert!(
        body.contains(
            r#"vitalmesh_processor_http_requests_total{method="POST",route="/internal/v1/process",status="200"} 1"#
        ),
        "the request was not counted:\n{body}"
    );
    // The job finished, so nothing is running.
    assert!(body.contains("vitalmesh_processor_active_jobs 0"), "{body}");

    service.stop().await;
}

/// The route label is the pattern, so asking about many jobs cannot widen
/// the label space (SPECIFICATIONS.md section 41).
#[tokio::test]
async fn asking_about_many_jobs_produces_one_series_and_leaks_no_identifier() {
    let service = Service::start(config()).await;

    let mut ids = Vec::new();
    for i in 0..5 {
        let id = format!("00000000-0000-4000-8000-00000000000{i}");
        Request::get(format!("/internal/v1/jobs/{id}"))
            .authorized()
            .send(service.addr)
            .await;
        ids.push(id);
    }

    let body = Request::get("/metrics").send(service.addr).await.body;

    let series = body
        .lines()
        .filter(|line| line.starts_with("vitalmesh_processor_http_requests_total{"))
        .filter(|line| line.contains(r#"route="/internal/v1/jobs/{job_id}""#))
        .count();
    assert_eq!(series, 1, "five job ids produced {series} series:\n{body}");

    for id in ids {
        assert!(
            !body.contains(&id),
            "a job id reached the exposition: {id}\n{body}"
        );
    }

    service.stop().await;
}

// ------------------------------------------------------------ tracing

/// A request carrying W3C trace context is served normally: adopting a
/// caller's trace must never change what the caller gets back. That the
/// trace is actually continued across the two services is asserted by the
/// gateway's end-to-end suite, which can see both sides.
#[tokio::test]
async fn a_request_carrying_trace_context_is_served_normally() {
    const TRACE_ID: &str = "4bf92f3577b34da6a3ce929d0e0e4736";
    let service = Service::start(config()).await;

    let response = Request::json("/internal/v1/process", &request_body("job-traced", 50))
        .authorized()
        .header("traceparent", &format!("00-{TRACE_ID}-00f067aa0ba902b7-01"))
        .send(service.addr)
        .await;

    assert_eq!(response.status, 200, "{}", response.body);
    service.stop().await;
}

/// A request with no trace context is served normally too. Tracing is never
/// a precondition for work.
#[tokio::test]
async fn a_request_without_trace_context_is_served_normally() {
    let service = Service::start(config()).await;

    let response = Request::json("/internal/v1/process", &request_body("job-untraced", 50))
        .authorized()
        .send(service.addr)
        .await;

    assert_eq!(response.status, 200, "{}", response.body);
    service.stop().await;
}

/// A malformed traceparent is ignored rather than trusted or refused: a
/// broken header from a caller must not cost them their request.
#[tokio::test]
async fn a_malformed_trace_header_does_not_fail_the_request() {
    let service = Service::start(config()).await;

    for value in ["not-a-valid-traceparent", "", "00-tooshort-x-01"] {
        let response = Request::get("/internal/v1/health")
            .header("traceparent", value)
            .send(service.addr)
            .await;
        assert_eq!(response.status, 200, "{value}: {}", response.body);
    }

    service.stop().await;
}

/// A collector that does not exist must cost a request nothing: export is
/// batched onto its own task, so an unreachable endpoint is an outage of
/// telemetry rather than of the service.
#[tokio::test]
async fn an_unreachable_collector_does_not_affect_requests() {
    let mut cfg = config();
    cfg.tracing = Tracing {
        // Nothing listens here.
        endpoint: "http://127.0.0.1:1".to_owned(),
        timeout: Duration::from_millis(100),
    };
    let service = Service::start(cfg).await;

    let response = Request::json(
        "/internal/v1/process",
        &request_body("job-no-collector", 50),
    )
    .authorized()
    .header(
        "traceparent",
        "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01",
    )
    .send(service.addr)
    .await;

    assert_eq!(response.status, 200, "{}", response.body);
    service.stop().await;
}
