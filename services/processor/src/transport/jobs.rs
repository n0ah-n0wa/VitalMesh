//! `GET /internal/v1/jobs/{job_id}`: this processor's view of one job.

use axum::Json;
use axum::extract::{Path, State};
use serde::Serialize;

use crate::VERSION;
use crate::domain::{AlgorithmVersion, JobId, JobStatus, Timestamp};
use crate::error::{Error, Kind, Result};
use crate::jobs::JobView;
use crate::state::SharedState;

/// The contract's job status response.
#[derive(Debug, Clone, Serialize, PartialEq, Eq)]
pub struct JobStatusResponse {
    pub job_id: JobId,
    pub status: JobStatus,
    pub algorithm_version: AlgorithmVersion,
    pub service_version: &'static str,
    pub requested_at: Timestamp,
    pub started_at: Option<Timestamp>,
    pub finished_at: Option<Timestamp>,
    /// Present only while the job runs.
    #[serde(skip_serializing_if = "Option::is_none")]
    pub running_for_ms: Option<u64>,
    /// Present and non-null only when the job failed.
    pub error: Option<JobError>,
}

/// Why a job failed, retained with its record so a failure never disappears
/// (SPECIFICATIONS.md section 94).
#[derive(Debug, Clone, Serialize, PartialEq, Eq)]
pub struct JobError {
    pub code: &'static str,
    pub message: String,
    pub retryable: bool,
}

/// Reports the job, or `404` when this processor holds no record of it.
///
/// A `404` means "not running here and no longer remembered", never "no such
/// job": the gateway's row is the system of record.
pub async fn get(
    State(state): State<SharedState>,
    Path(job_id): Path<String>,
) -> Result<Json<JobStatusResponse>> {
    let job_id = JobId::parse(&job_id).map_err(|cause| {
        Error::new(
            Kind::Invalid,
            "INVALID_REQUEST",
            "The job id must be 1 to 128 characters from [A-Za-z0-9._-].",
        )
        .with_source(cause)
    })?;

    let view = state.jobs.get(&job_id).ok_or_else(|| {
        Error::new(
            Kind::NotFound,
            "JOB_NOT_FOUND",
            "This processor holds no record of that job.",
        )
    })?;
    Ok(Json(response(view)))
}

fn response(view: JobView) -> JobStatusResponse {
    JobStatusResponse {
        job_id: view.id,
        status: view.status,
        algorithm_version: view.algorithm_version,
        service_version: VERSION,
        requested_at: view.requested_at,
        started_at: view.started_at,
        finished_at: view.finished_at,
        running_for_ms: view.running_for.map(|d| d.as_millis() as u64),
        error: view.failure.map(|failure| JobError {
            code: failure.code,
            message: failure.message,
            retryable: failure.retryable,
        }),
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::jobs::Failure;
    use std::time::Duration;

    fn view(status: JobStatus) -> JobView {
        JobView {
            id: JobId::parse("job-1").unwrap(),
            status,
            algorithm_version: AlgorithmVersion::new(1, 0, 0),
            requested_at: Timestamp::parse("2026-09-06T12:00:00Z").unwrap(),
            started_at: Timestamp::parse("2026-09-06T12:00:01Z").ok(),
            finished_at: None,
            running_for: None,
            failure: None,
        }
    }

    #[test]
    fn a_running_job_reports_its_age_and_no_error() {
        let mut running = view(JobStatus::Processing);
        running.running_for = Some(Duration::from_millis(1420));
        let body = response(running);
        assert_eq!(body.status, JobStatus::Processing);
        assert_eq!(body.running_for_ms, Some(1420));
        assert!(body.error.is_none());
        assert!(body.finished_at.is_none());

        let json = serde_json::to_value(&body).unwrap();
        assert_eq!(json["running_for_ms"], 1420);
        assert_eq!(json["error"], serde_json::Value::Null);
        assert_eq!(json["status"], "PROCESSING");
    }

    #[test]
    fn a_finished_job_omits_the_age_entirely() {
        let mut finished = view(JobStatus::Completed);
        finished.finished_at = Timestamp::parse("2026-09-06T12:00:09Z").ok();
        let json = serde_json::to_value(response(finished)).unwrap();
        assert!(
            json.get("running_for_ms").is_none(),
            "a finished job must not claim to be running"
        );
        assert_eq!(json["finished_at"], "2026-09-06T12:00:09Z");
    }

    #[test]
    fn a_failed_job_carries_its_diagnosis() {
        let mut failed = view(JobStatus::Failed);
        failed.failure = Some(Failure {
            code: "PROCESSING_TIMEOUT",
            message: "The work exceeded its time limit.".to_owned(),
            retryable: true,
        });
        let json = serde_json::to_value(response(failed)).unwrap();
        assert_eq!(json["status"], "FAILED");
        assert_eq!(json["error"]["code"], "PROCESSING_TIMEOUT");
        assert_eq!(json["error"]["retryable"], true);
    }
}
