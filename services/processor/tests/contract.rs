//! Server-side conformance with the internal API contract
//! (`contracts/internal-api/processor-v1.json`, SPECIFICATIONS.md section
//! 49).
//!
//! The gateway checks that the document is a well-formed OpenAPI description
//! whose examples satisfy its own schemas. That says nothing about whether
//! this service agrees with it. These tests close that gap from the server
//! side by confronting the document with the types the processor really
//! serialises: every enumeration in the contract is compared with the wire
//! values of the corresponding domain type, and the contract's examples are
//! deserialised into the real request and result types, which apply every
//! domain invariant on the way in.
//!
//! What this cannot check is the wiring, because there is none yet: the
//! routes named in the contract are not served (see the contract's README).
//! When they are, a live contract test replaces the deserialisation here.

use std::collections::BTreeSet;
use std::fs;
use std::path::{Path, PathBuf};

use serde_json::Value;

use processor::anomaly::ALGORITHM_VERSION;
use processor::domain::{
    AnomalyMetric, JobStatus, MeasurementType, ProcessingResult, Severity, Unit, Window,
};
use processor::pipeline::{Outcome, RejectionCode, Request};

fn contract_path() -> PathBuf {
    Path::new(env!("CARGO_MANIFEST_DIR")).join("../../contracts/internal-api/processor-v1.json")
}

fn contract() -> Value {
    let path = contract_path();
    let raw = fs::read_to_string(&path).unwrap_or_else(|e| panic!("read {}: {e}", path.display()));
    serde_json::from_str(&raw).unwrap_or_else(|e| panic!("parse {}: {e}", path.display()))
}

/// The `enum` of one component schema.
fn schema_enum(contract: &Value, name: &str) -> Vec<String> {
    let values = contract["components"]["schemas"][name]["enum"]
        .as_array()
        .unwrap_or_else(|| panic!("components.schemas.{name}.enum is missing"));
    values
        .iter()
        .map(|v| {
            v.as_str()
                .unwrap_or_else(|| panic!("components.schemas.{name}.enum holds a non-string"))
                .to_owned()
        })
        .collect()
}

/// How a value is spelled on the wire, according to serde.
fn wire<T: serde::Serialize>(value: &T) -> String {
    match serde_json::to_value(value).expect("serialise") {
        Value::String(text) => text,
        other => panic!("expected a string on the wire, got {other}"),
    }
}

fn assert_same_values(name: &str, contract: Vec<String>, actual: Vec<String>) {
    let declared: BTreeSet<_> = contract.iter().cloned().collect();
    let served: BTreeSet<_> = actual.iter().cloned().collect();
    assert_eq!(
        declared,
        served,
        "the contract's {name} does not match what the processor serialises.\n  \
         only in the contract: {:?}\n  only in the service:  {:?}",
        declared.difference(&served).collect::<Vec<_>>(),
        served.difference(&declared).collect::<Vec<_>>()
    );
    assert_eq!(
        contract.len(),
        declared.len(),
        "the contract's {name} repeats a value"
    );
}

#[test]
fn measurement_types_match() {
    let actual = MeasurementType::ALL.iter().map(wire).collect();
    assert_same_values(
        "MeasurementType",
        schema_enum(&contract(), "MeasurementType"),
        actual,
    );
}

#[test]
fn units_match() {
    // Every canonical unit the domain can produce. A type whose unit is not
    // in the contract would make an accepted reading undescribable.
    let actual = MeasurementType::ALL
        .iter()
        .map(|t| wire(&t.canonical_unit()))
        .collect::<BTreeSet<_>>()
        .into_iter()
        .collect::<Vec<_>>();
    let declared: BTreeSet<String> = schema_enum(&contract(), "Unit").into_iter().collect();
    for unit in &actual {
        assert!(
            declared.contains(unit),
            "the canonical unit {unit:?} is missing from the contract's Unit enum"
        );
    }
    for unit in &declared {
        assert!(
            actual.contains(unit),
            "the contract declares the unit {unit:?}, which no measurement type uses"
        );
    }
    // The symbols must be the exact serde spelling, not merely present.
    for symbol in [
        Unit::BeatsPerMinute,
        Unit::MillimetresOfMercury,
        Unit::Percent,
        Unit::Celsius,
        Unit::MilligramsPerDecilitre,
        Unit::BreathsPerMinute,
    ] {
        assert_eq!(
            wire(&symbol),
            symbol.symbol(),
            "serde and symbol() disagree"
        );
    }
}

#[test]
fn windows_match() {
    let actual = Window::ALL.iter().map(wire).collect();
    assert_same_values("Window", schema_enum(&contract(), "Window"), actual);
}

#[test]
fn job_statuses_match() {
    let actual = [
        JobStatus::Pending,
        JobStatus::Processing,
        JobStatus::Completed,
        JobStatus::Failed,
        JobStatus::Cancelled,
    ]
    .iter()
    .map(wire)
    .collect();
    assert_same_values(
        "ProcessingStatus",
        schema_enum(&contract(), "ProcessingStatus"),
        actual,
    );
}

#[test]
fn severities_and_metrics_match() {
    let severities = [Severity::Info, Severity::Warning, Severity::Critical]
        .iter()
        .map(wire)
        .collect();
    assert_same_values("Severity", schema_enum(&contract(), "Severity"), severities);

    let metrics = [
        AnomalyMetric::Threshold,
        AnomalyMetric::ZScore,
        AnomalyMetric::RollingDeviation,
    ]
    .iter()
    .map(wire)
    .collect();
    assert_same_values(
        "AnomalyMetric",
        schema_enum(&contract(), "AnomalyMetric"),
        metrics,
    );
}

#[test]
fn rejection_codes_match() {
    let actual = [
        RejectionCode::InvalidId,
        RejectionCode::UnknownType,
        RejectionCode::UnknownUnit,
        RejectionCode::UnitMismatch,
        RejectionCode::NonFiniteValue,
        RejectionCode::ValueOutOfRange,
        RejectionCode::InvalidTimestamp,
        RejectionCode::TimestampOutOfBounds,
        RejectionCode::DuplicateId,
    ]
    .iter()
    .map(wire)
    .collect();
    assert_same_values(
        "RejectionCode",
        schema_enum(&contract(), "RejectionCode"),
        actual,
    );
}

/// The algorithm version the contract advertises must be the one this build
/// implements, otherwise the gateway would pin a version every job is
/// refused for.
#[test]
fn advertised_algorithm_version_is_the_one_implemented() {
    let contract = contract();
    let example = &contract["paths"]["/internal/v1/health"]["get"]["responses"]["200"]["content"]["application/json"]
        ["example"];
    let advertised = example["algorithm_version"]
        .as_str()
        .expect("the health example must advertise an algorithm version");
    assert_eq!(
        advertised,
        ALGORITHM_VERSION.to_string(),
        "the contract advertises algorithm version {advertised} but this build implements {ALGORITHM_VERSION}"
    );

    // The health example also names the service and the contract version it
    // implements; the latter must be the document's own version.
    assert_eq!(
        example["service"].as_str(),
        Some(processor::SERVICE_NAME),
        "the health example names a different service"
    );
    assert_eq!(
        example["contract_version"].as_str(),
        contract["info"]["version"].as_str(),
        "the health example advertises a contract version other than info.version"
    );
}

/// The version this build reports to the gateway must be the version of the
/// document it was written against. A contract edited without updating the
/// constant would have the processor claiming to implement something it does
/// not.
#[test]
fn the_service_reports_the_contract_version_it_implements() {
    let contract = contract();
    assert_eq!(
        Some(processor::CONTRACT_VERSION),
        contract["info"]["version"].as_str(),
        "processor::CONTRACT_VERSION and the contract's info.version disagree"
    );
}

/// Every route the contract defines must be one the service serves, with the
/// method the contract gives it. The router is the production one.
#[tokio::test]
async fn every_operation_in_the_contract_is_served() {
    use axum::body::Body;
    use axum::http::Request as HttpRequest;
    use tower::ServiceExt;

    let contract = contract();
    let paths = contract["paths"].as_object().expect("paths");
    assert!(!paths.is_empty(), "the contract defines no paths");

    for (path, item) in paths {
        for method in ["get", "post", "put", "delete", "patch"] {
            if item.get(method).is_none() {
                continue;
            }
            // A concrete request for the templated path.
            let target = path.replace("{job_id}", "some-job");
            let request = HttpRequest::builder()
                .method(method.to_uppercase().as_str())
                .uri(&target)
                .header("content-type", "application/json")
                .body(Body::empty())
                .expect("build request");

            let response = processor::transport::router(state())
                .oneshot(request)
                .await
                .expect("route");
            let (status, code) = outcome_of(response).await;
            assert!(
                served(status, &code),
                "{} {target} is in the contract but the router answers {status} {code}",
                method.to_uppercase()
            );
        }
    }
}

/// The service must serve nothing beyond the contract and the unversioned
/// platform probes: an undocumented internal endpoint is surface nobody
/// reviewed.
#[tokio::test]
async fn no_internal_route_exists_outside_the_contract() {
    use axum::body::Body;
    use axum::http::Request as HttpRequest;
    use tower::ServiceExt;

    let contract = contract();
    let documented: BTreeSet<String> = contract["paths"]
        .as_object()
        .expect("paths")
        .keys()
        .cloned()
        .collect();

    // Paths a reviewer might expect to exist. Any that answers must be in
    // the contract.
    let candidates = [
        "/internal/v1/process",
        "/internal/v1/jobs/some-job",
        "/internal/v1/health",
        "/internal/v1/jobs",
        "/internal/v1/jobs/some-job/cancel",
        "/internal/v1/results",
        "/internal/v1/metrics",
        "/internal/v2/process",
    ];
    for target in candidates {
        for method in ["GET", "POST"] {
            let request = HttpRequest::builder()
                .method(method)
                .uri(target)
                .header("content-type", "application/json")
                .body(Body::empty())
                .expect("build request");
            let response = processor::transport::router(state())
                .oneshot(request)
                .await
                .expect("route");
            let (status, code) = outcome_of(response).await;
            if !served(status, &code) {
                continue;
            }
            let templated = target.replace("some-job", "{job_id}");
            assert!(
                documented.contains(&templated),
                "{method} {target} is served but is not in the contract"
            );
        }
    }
}

/// The status and envelope code of a response, so that a routing failure can
/// be told from a handler's own answer.
async fn outcome_of(response: axum::response::Response) -> (u16, String) {
    use axum::body::to_bytes;

    let status = response.status().as_u16();
    let bytes = to_bytes(response.into_body(), 1 << 20)
        .await
        .expect("read the body");
    let code = serde_json::from_slice::<Value>(&bytes)
        .ok()
        .and_then(|body| body["error"]["code"].as_str().map(str::to_owned))
        .unwrap_or_default();
    (status, code)
}

/// Whether a route exists at all. `NOT_FOUND` and `METHOD_NOT_ALLOWED` are
/// the codes the router's own fallbacks emit; a handler that answers
/// `JOB_NOT_FOUND` is a route that exists and did its job.
fn served(status: u16, code: &str) -> bool {
    !matches!(
        (status, code),
        (404, "NOT_FOUND") | (405, "METHOD_NOT_ALLOWED")
    )
}

/// State for the router tests: no credential, so a route that exists answers
/// rather than demanding one, which is what these two tests ask about.
fn state() -> std::sync::Arc<processor::state::AppState> {
    use tokio_util::sync::CancellationToken;
    let config = processor::config::Config::load(|_| None).expect("defaults must load");
    std::sync::Arc::new(processor::state::AppState::new(
        config,
        CancellationToken::new(),
    ))
}

/// The request example must be a request this processor accepts. Building
/// the real type applies every domain rule: identifier shapes, unit and type
/// agreement, value ranges, timestamp parsing, parameter non-emptiness and
/// the job's timestamp ordering.
#[test]
fn the_request_example_deserialises_into_the_real_request_type() {
    let contract = contract();
    let example = &contract["paths"]["/internal/v1/process"]["post"]["requestBody"]["content"]["application/json"]
        ["example"];
    let request: Request = serde_json::from_value(example.clone())
        .expect("the contract's process request example must be a request the processor accepts");

    assert_eq!(
        request.job.algorithm_version(),
        ALGORITHM_VERSION,
        "the example asks for an algorithm version this build would refuse"
    );
    assert!(
        !request.readings.is_empty(),
        "the example carries no readings"
    );
}

/// The success example must be an outcome this processor could have
/// produced. `ProcessingResult` validates on construction that every anomaly
/// belongs to the result's type, window and version and lies inside the
/// window, and `Statistics` validates the relations between the moments, so
/// this rejects an example with invented numbers.
#[test]
fn the_outcome_example_deserialises_into_the_real_result_types() {
    let contract = contract();
    let example = &contract["paths"]["/internal/v1/process"]["post"]["responses"]["200"]["content"]
        ["application/json"]["example"];

    // `Outcome` is serialise-only, so check the part that carries the
    // domain invariants: the results themselves.
    let results: Vec<ProcessingResult> = serde_json::from_value(example["results"].clone())
        .expect("the contract's outcome example must hold results the processor could produce");
    assert!(!results.is_empty(), "the outcome example has no results");

    for result in &results {
        assert_eq!(
            result.algorithm_version(),
            ALGORITHM_VERSION,
            "a result in the example carries a foreign algorithm version"
        );
        for anomaly in result.anomalies() {
            assert_eq!(anomaly.window, result.window());
            assert_eq!(anomaly.measurement_type, result.measurement_type());
        }
    }

    // The accounting the contract documents must hold in its own example.
    let accepted = example["accepted"].as_u64().expect("accepted");
    let skipped = example["skipped"].as_u64().expect("skipped");
    let rejected = example["rejected"].as_array().expect("rejected").len() as u64;
    let request = &contract["paths"]["/internal/v1/process"]["post"]["requestBody"]["content"]["application/json"]
        ["example"];
    let sent = request["readings"].as_array().expect("readings").len() as u64;
    assert_eq!(
        accepted + skipped + rejected,
        sent,
        "the example's accounting does not add up: {accepted} accepted + {skipped} skipped + \
         {rejected} rejected != {sent} sent"
    );

    // The outcome type is what the pipeline returns; make sure the example
    // shape and the serialised shape agree field for field.
    let outcome_fields: BTreeSet<String> = example
        .as_object()
        .expect("the outcome example is an object")
        .keys()
        .cloned()
        .collect();
    let serialised = serde_json::to_value(sample_outcome()).expect("serialise an outcome");
    let actual_fields: BTreeSet<String> = serialised
        .as_object()
        .expect("an outcome serialises to an object")
        .keys()
        .cloned()
        .collect();
    assert_eq!(
        outcome_fields, actual_fields,
        "the contract's outcome example and the Outcome type have different fields"
    );
}

/// Every number in the success example must be a number the engine really
/// computes. Running the pipeline over the contract's own request example
/// must reproduce the contract's own outcome example exactly, statistic for
/// statistic and window for window.
///
/// Anomalies are excluded from the comparison because which values are
/// flagged depends on the rule configuration, which is deployment
/// configuration rather than part of this contract; the example is
/// illustrated with a threshold rule whose upper warning bound is 180. The
/// statistics, the window boundaries, the ordering and the accounting are
/// fully determined by the request and are all compared.
#[test]
fn the_engine_reproduces_the_outcome_example() {
    let contract = contract();
    let request_example = &contract["paths"]["/internal/v1/process"]["post"]["requestBody"]["content"]
        ["application/json"]["example"];
    let outcome_example = &contract["paths"]["/internal/v1/process"]["post"]["responses"]["200"]["content"]
        ["application/json"]["example"];

    let request: Request =
        serde_json::from_value(request_example.clone()).expect("the request example must parse");
    let service_version = outcome_example["service_version"]
        .as_str()
        .expect("the outcome example must name a service version");

    let produced = run_without_rules(request, service_version);
    let mut produced = serde_json::to_value(produced).expect("serialise the outcome");
    let mut expected = outcome_example.clone();
    for document in [&mut produced, &mut expected] {
        for result in document["results"]
            .as_array_mut()
            .expect("results is an array")
        {
            result["anomalies"] = Value::Array(Vec::new());
        }
    }

    assert_eq!(
        produced, expected,
        "the pipeline does not reproduce the contract's success example; the example was not \
         generated from the engine, or the engine changed"
    );
}

/// Runs one request through a pipeline with no rules, so the outcome holds
/// statistics and accounting but no anomalies.
fn run_without_rules(request: Request, service_version: &str) -> Outcome {
    use std::num::NonZeroUsize;
    use std::time::Duration;

    use processor::anomaly::{Detector, RuleSet};
    use processor::domain::TimestampBounds;
    use processor::pipeline::{Control, Limits, Pipeline};

    Pipeline::new(
        Detector::new(RuleSet::empty()),
        Limits {
            max_job_measurements: NonZeroUsize::new(100_000).unwrap(),
            timestamp_bounds: TimestampBounds {
                max_future_skew: Duration::from_secs(300),
                max_age: Duration::from_secs(400 * 24 * 3600),
            },
        },
        service_version,
    )
    .run(request, &Control::unbounded())
    .expect("the contract's example job must succeed")
}

/// An outcome produced by the real pipeline, used to compare field names.
fn sample_outcome() -> Outcome {
    use std::num::NonZeroUsize;
    use std::time::Duration;

    use processor::anomaly::{Detector, RuleSet};
    use processor::domain::{
        JobId, PatientId, Percentile, ProcessingJob, ProcessingParameters, Timestamp,
        TimestampBounds,
    };
    use processor::pipeline::{Control, Limits, Pipeline, RawReading};

    let pipeline = Pipeline::new(
        Detector::new(RuleSet::empty()),
        Limits {
            max_job_measurements: NonZeroUsize::new(100).unwrap(),
            timestamp_bounds: TimestampBounds {
                max_future_skew: Duration::from_secs(300),
                max_age: Duration::from_secs(30 * 24 * 3600),
            },
        },
        "contract-test",
    );
    let request = Request {
        job: ProcessingJob::new(
            JobId::parse("job-1").unwrap(),
            PatientId::parse("patient-1").unwrap(),
            ProcessingParameters::new(
                [MeasurementType::HeartRate],
                [Window::OneHour],
                [Percentile::new(50).unwrap()],
            )
            .unwrap(),
            ALGORITHM_VERSION,
            Timestamp::parse("2026-09-06T12:00:00Z").unwrap(),
        ),
        readings: vec![RawReading {
            id: "m-1".into(),
            measurement_type: "HEART_RATE".into(),
            value: 72.0,
            unit: "bpm".into(),
            recorded_at: "2026-09-06T11:30:00Z".into(),
        }],
    };
    pipeline
        .run(request, &Control::unbounded())
        .expect("the sample job must succeed")
}

/// Every error code the contract promises must be one this service can
/// actually produce, or one the transport layer generates. A code in the
/// document that no code path emits is a promise the gateway would branch on
/// and never see.
#[test]
fn declared_error_codes_are_ones_the_service_can_emit() {
    // Codes the service constructs itself or that its transport generates
    // from a framework rejection. Kept explicit so that adding a code to the
    // contract forces a look at whether anything emits it.
    let emitted: BTreeSet<&str> = [
        // processor::error constructors.
        "INTERNAL_ERROR",
        "PROCESSOR_OVERLOADED",
        "PROCESSING_TIMEOUT",
        "PROCESSING_CANCELLED",
        // engine and pipeline.
        "PROCESSOR_SHUTTING_DOWN",
        "JOB_ALREADY_RUNNING",
        "UNSUPPORTED_ALGORITHM_VERSION",
        "JOB_TOO_LARGE",
        "NO_VALID_MEASUREMENTS",
        // transport::error, from framework rejections and fallbacks.
        "INVALID_REQUEST",
        "REQUEST_BODY_TOO_LARGE",
        "UNSUPPORTED_MEDIA_TYPE",
        "VALIDATION_FAILED",
        "NOT_FOUND",
        "METHOD_NOT_ALLOWED",
        // Reserved for the routes this contract defines but that are not
        // wired yet; see the contract's README.
        "JOB_NOT_FOUND",
        "UNAUTHENTICATED",
    ]
    .into_iter()
    .collect();

    let contract = contract();
    let mut declared: BTreeSet<String> = BTreeSet::new();
    collect_error_codes(&contract, &mut declared);
    assert!(
        !declared.is_empty(),
        "the contract declares no error codes; the walk is not working"
    );
    for code in &declared {
        assert!(
            emitted.contains(code.as_str()),
            "the contract promises the error code {code:?}, which nothing in the processor emits"
        );
    }
}

fn collect_error_codes(value: &Value, out: &mut BTreeSet<String>) {
    match value {
        Value::Object(map) => {
            if let Some(Value::Array(codes)) = map.get("x-error-codes") {
                out.extend(codes.iter().filter_map(|c| c.as_str().map(str::to_owned)));
            }
            for item in map.values() {
                collect_error_codes(item, out);
            }
        }
        Value::Array(items) => items.iter().for_each(|i| collect_error_codes(i, out)),
        _ => {}
    }
}

/// The limits the contract advertises must be the service's own defaults, so
/// a client sizing its requests from the health response is sizing them
/// correctly.
#[test]
fn advertised_limits_match_the_configuration_defaults() {
    use processor::config::Config;

    let config = Config::load(|_| None).expect("defaults must load");
    let contract = contract();
    let limits = &contract["paths"]["/internal/v1/health"]["get"]["responses"]["200"]["content"]["application/json"]
        ["example"]["limits"];

    assert_eq!(
        limits["max_job_measurements"].as_u64(),
        Some(config.processing.max_job_measurements.get() as u64),
        "MAX_JOB_MEASUREMENTS"
    );
    assert_eq!(
        limits["max_request_bytes"].as_u64(),
        Some(config.http.max_body_bytes as u64),
        "HTTP_MAX_BODY_BYTES"
    );
    assert_eq!(
        limits["processing_timeout_ms"].as_u64(),
        Some(config.processing.timeout.as_millis() as u64),
        "PROCESSING_TIMEOUT"
    );
    assert_eq!(
        limits["max_concurrent_jobs"].as_u64(),
        Some(config.processing.max_concurrent_jobs.get() as u64),
        "MAX_CONCURRENT_JOBS"
    );
    assert_eq!(
        limits["job_retention_seconds"].as_u64(),
        Some(config.processing.job_retention.as_secs()),
        "JOB_RETENTION"
    );

    // The contract's own ceiling on a request's readings must not promise
    // more than the service would accept by default.
    let max_items =
        contract["components"]["schemas"]["ProcessRequest"]["properties"]["readings"]["maxItems"]
            .as_u64()
            .expect("ProcessRequest.readings.maxItems");
    assert_eq!(
        max_items,
        config.processing.max_job_measurements.get() as u64,
        "the contract's maxItems and MAX_JOB_MEASUREMENTS disagree"
    );
}
