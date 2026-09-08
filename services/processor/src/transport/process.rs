//! `POST /internal/v1/process`: run one job's measurements through the
//! pipeline and return its results.

use std::time::Duration;

use axum::Json;
use axum::body::Bytes;
use axum::extract::State;
use axum::http::HeaderMap;

use crate::error::{Error, Kind, Result};
use crate::pipeline::{Outcome, Request};
use crate::state::SharedState;

use super::body;

/// Header through which a client declares how long it is still prepared to
/// wait, so that this processor does not keep working on something nobody is
/// listening for.
pub const REQUEST_TIMEOUT_HEADER: &str = "x-request-timeout-ms";

/// Runs the job.
///
/// The body is read as bytes and parsed here rather than through the JSON
/// extractor, because the contract is precise about which failures are which:
/// anything the request could not even be understood as is `400`, and only a
/// well-formed request that breaks a rule is `422`. The framework's own
/// mapping answers `422` for a type error, which would blur the two.
pub async fn process(
    State(state): State<SharedState>,
    headers: HeaderMap,
    payload: Bytes,
) -> Result<Json<Outcome>> {
    body::require_json(&headers)?;
    let request: Request = body::parse(&payload)?;
    let budget = timeout_budget(&headers)?;

    let job_id = request.job.id().clone();
    let algorithm_version = request.job.algorithm_version();
    let requested_at = request.job.requested_at();
    let readings = request.readings.len();

    // Registering first means a duplicate id is refused without disturbing
    // the attempt already in flight, and that the job is observable through
    // the jobs endpoint for as long as it runs.
    let running = state.jobs.start(&job_id, algorithm_version, requested_at)?;

    let span = tracing::info_span!(
        "process",
        job_id = %job_id,
        algorithm_version = %algorithm_version,
        readings,
    );
    let _entered = span.enter();

    let started = std::time::Instant::now();
    match state.processor.process_within(request, budget).await {
        Ok(outcome) => {
            running.completed();
            state.metrics.job(
                crate::metrics::OUTCOME_COMPLETED,
                started.elapsed().as_secs_f64(),
            );
            tracing::info!(
                status = "COMPLETED",
                duration_ms = started.elapsed().as_millis() as u64,
                accepted = outcome.accepted,
                skipped = outcome.skipped,
                rejected = outcome.rejected.len(),
                results = outcome.results.len(),
                anomalies = outcome
                    .results
                    .iter()
                    .map(|r| r.anomalies().len())
                    .sum::<usize>(),
                "job processed"
            );
            Ok(Json(outcome))
        }
        Err(error) => {
            // A job the engine never admitted did not run, so it leaves no
            // record; anything else ended and keeps its diagnosis.
            if admission_refused(&error) {
                running.never_started();
                // A refused job never ran, so it is counted apart from the
                // jobs that did and contributes no duration.
                state.metrics.job_refused();
            } else {
                running.ended(&error);
                let outcome = if error.kind() == Kind::Cancelled {
                    crate::metrics::OUTCOME_CANCELLED
                } else {
                    crate::metrics::OUTCOME_FAILED
                };
                state.metrics.job(outcome, started.elapsed().as_secs_f64());
            }
            tracing::info!(
                status = %if error.kind() == Kind::Cancelled { "CANCELLED" } else { "FAILED" },
                duration_ms = started.elapsed().as_millis() as u64,
                code = error.code(),
                retryable = error.is_retryable(),
                "job did not complete"
            );
            Err(error)
        }
    }
}

/// Whether the engine refused the work rather than running it.
fn admission_refused(error: &Error) -> bool {
    matches!(
        error.kind(),
        Kind::Overloaded | Kind::Unavailable | Kind::Conflict
    )
}

/// The client's remaining budget, when it sent one. An unparseable or
/// out-of-range value is a malformed request rather than something to guess
/// at: a client that means to bound the work must be told its bound was not
/// understood.
fn timeout_budget(headers: &HeaderMap) -> Result<Option<Duration>> {
    let Some(value) = headers.get(REQUEST_TIMEOUT_HEADER) else {
        return Ok(None);
    };
    let invalid = || {
        Error::new(
            Kind::Invalid,
            "INVALID_REQUEST",
            "X-Request-Timeout-Ms must be a whole number of milliseconds between 1 and 3600000.",
        )
    };
    let millis: u64 = value
        .to_str()
        .ok()
        .and_then(|raw| raw.trim().parse().ok())
        .ok_or_else(invalid)?;
    if !(1..=3_600_000).contains(&millis) {
        return Err(invalid());
    }
    Ok(Some(Duration::from_millis(millis)))
}

#[cfg(test)]
mod tests {
    use super::*;
    use axum::http::HeaderValue;

    fn headers(value: &str) -> HeaderMap {
        let mut headers = HeaderMap::new();
        headers.insert(
            REQUEST_TIMEOUT_HEADER,
            HeaderValue::from_str(value).unwrap(),
        );
        headers
    }

    #[test]
    fn a_missing_budget_is_not_an_error() {
        assert_eq!(timeout_budget(&HeaderMap::new()).unwrap(), None);
    }

    #[test]
    fn a_budget_is_read_as_milliseconds() {
        assert_eq!(
            timeout_budget(&headers("1500")).unwrap(),
            Some(Duration::from_millis(1500))
        );
        assert_eq!(
            timeout_budget(&headers(" 1 ")).unwrap(),
            Some(Duration::from_millis(1))
        );
        assert_eq!(
            timeout_budget(&headers("3600000")).unwrap(),
            Some(Duration::from_millis(3_600_000))
        );
    }

    #[test]
    fn an_unusable_budget_is_rejected_rather_than_ignored() {
        for bad in [
            "0",
            "-1",
            "1.5",
            "abc",
            "",
            "3600001",
            "99999999999999999999",
        ] {
            let error =
                timeout_budget(&headers(bad)).expect_err(&format!("{bad:?} should be rejected"));
            assert_eq!(error.kind(), Kind::Invalid);
            assert_eq!(error.code(), "INVALID_REQUEST");
        }
    }

    #[test]
    fn only_admission_failures_count_as_never_started() {
        assert!(admission_refused(&Error::overloaded()));
        assert!(admission_refused(&Error::new(
            Kind::Unavailable,
            "PROCESSOR_SHUTTING_DOWN",
            "shutting down"
        )));
        assert!(admission_refused(&Error::new(
            Kind::Conflict,
            "JOB_ALREADY_RUNNING",
            "already running"
        )));
        assert!(!admission_refused(&Error::timeout()));
        assert!(!admission_refused(&Error::cancelled()));
        assert!(!admission_refused(&Error::new(
            Kind::Validation,
            "JOB_TOO_LARGE",
            "too large"
        )));
    }
}
