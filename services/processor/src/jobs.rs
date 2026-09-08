//! The bounded registry of jobs this processor knows about.
//!
//! `GET /internal/v1/jobs/{job_id}` answers from here. The registry is an
//! *observation* of work this instance was asked to do, never a system of
//! record: the gateway owns the authoritative job row (SPECIFICATIONS.md
//! sections 6.1 and 92). A job this instance never received, or one whose
//! record has been evicted, is simply absent.
//!
//! # Bounds
//!
//! Nothing here may grow without limit (SPECIFICATIONS.md section 91).
//! Running jobs are bounded by the engine's `MAX_CONCURRENT_JOBS`. Finished
//! jobs are kept for `JOB_RETENTION` and, independently, the newest
//! [`MAX_RETAINED`] of them; whichever bound bites first applies. Eviction
//! runs on every write, so a busy processor sheds old records without a
//! background task.

use std::collections::HashMap;
use std::sync::{Mutex, MutexGuard, PoisonError};
use std::time::{Duration, Instant};

use crate::domain::{AlgorithmVersion, JobId, JobStatus, Timestamp};
use crate::error::{Error, Kind, Result};

/// Largest number of finished jobs kept, whatever the retention window says.
/// A record is a few hundred bytes, so this is well under a megabyte.
pub const MAX_RETAINED: usize = 1024;

/// Why a job ended, as far as this processor saw it.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct Failure {
    pub code: &'static str,
    pub message: String,
    pub retryable: bool,
}

impl Failure {
    fn from_error(error: &Error) -> Self {
        Self {
            code: error.code(),
            message: error.message().to_owned(),
            retryable: error.is_retryable(),
        }
    }
}

/// One job as this processor saw it.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct JobView {
    pub id: JobId,
    pub status: JobStatus,
    pub algorithm_version: AlgorithmVersion,
    pub requested_at: Timestamp,
    /// When this processor admitted the job. `None` only when the clock was
    /// unusable at that moment, which the timestamp type reports rather than
    /// guessing.
    pub started_at: Option<Timestamp>,
    pub finished_at: Option<Timestamp>,
    /// How long the job has been running, present only while it runs.
    pub running_for: Option<Duration>,
    pub failure: Option<Failure>,
}

#[derive(Debug)]
struct Record {
    status: JobStatus,
    algorithm_version: AlgorithmVersion,
    requested_at: Timestamp,
    started_at: Option<Timestamp>,
    started_instant: Instant,
    finished_at: Option<Timestamp>,
    finished_instant: Option<Instant>,
    failure: Option<Failure>,
}

/// The jobs this processor is running or recently ran.
#[derive(Debug)]
pub struct JobRegistry {
    jobs: Mutex<HashMap<JobId, Record>>,
    retention: Duration,
}

impl JobRegistry {
    pub fn new(retention: Duration) -> Self {
        Self {
            jobs: Mutex::new(HashMap::new()),
            retention,
        }
    }

    pub fn retention(&self) -> Duration {
        self.retention
    }

    /// Registers `id` as running and returns the guard that must record how
    /// it ended.
    ///
    /// Fails with a conflict when the same id is already running here, which
    /// is the same answer the engine gives; checking it first means a
    /// duplicate never disturbs the record of the attempt already in flight.
    pub fn start(
        &self,
        id: &JobId,
        algorithm_version: AlgorithmVersion,
        requested_at: Timestamp,
    ) -> Result<RunningJob<'_>> {
        let now = Instant::now();
        let mut jobs = self.lock();
        if jobs
            .get(id)
            .is_some_and(|job| job.status == JobStatus::Processing)
        {
            return Err(Error::new(
                Kind::Conflict,
                "JOB_ALREADY_RUNNING",
                "A job with this id is already running.",
            ));
        }
        evict(&mut jobs, self.retention, now);
        jobs.insert(
            id.clone(),
            Record {
                status: JobStatus::Processing,
                algorithm_version,
                requested_at,
                started_at: Timestamp::now(),
                started_instant: now,
                finished_at: None,
                finished_instant: None,
                failure: None,
            },
        );
        Ok(RunningJob {
            registry: self,
            id: id.clone(),
            settled: false,
        })
    }

    /// This processor's view of `id`, or `None` when it holds no record.
    pub fn get(&self, id: &JobId) -> Option<JobView> {
        let now = Instant::now();
        let mut jobs = self.lock();
        evict(&mut jobs, self.retention, now);
        let record = jobs.get(id)?;
        Some(JobView {
            id: id.clone(),
            status: record.status,
            algorithm_version: record.algorithm_version,
            requested_at: record.requested_at,
            started_at: record.started_at,
            finished_at: record.finished_at,
            running_for: (record.status == JobStatus::Processing)
                .then(|| now.saturating_duration_since(record.started_instant)),
            failure: record.failure.clone(),
        })
    }

    /// How many records are held, running and finished.
    pub fn len(&self) -> usize {
        self.lock().len()
    }

    pub fn is_empty(&self) -> bool {
        self.len() == 0
    }

    fn settle(&self, id: &JobId, status: JobStatus, failure: Option<Failure>) {
        let now = Instant::now();
        let mut jobs = self.lock();
        if let Some(record) = jobs.get_mut(id) {
            record.status = status;
            record.finished_at = Timestamp::now();
            record.finished_instant = Some(now);
            record.failure = failure;
        }
        evict(&mut jobs, self.retention, now);
    }

    fn forget(&self, id: &JobId) {
        self.lock().remove(id);
    }

    fn lock(&self) -> MutexGuard<'_, HashMap<JobId, Record>> {
        // A panic never leaves the map half-updated, so a poisoned lock is
        // safe to keep using.
        self.jobs.lock().unwrap_or_else(PoisonError::into_inner)
    }
}

/// Drops finished records that are older than the retention window, then, if
/// too many remain, the oldest of them. Running records are never evicted.
fn evict(jobs: &mut HashMap<JobId, Record>, retention: Duration, now: Instant) {
    jobs.retain(|_, record| match record.finished_instant {
        Some(finished) => now.saturating_duration_since(finished) < retention,
        None => true,
    });
    let finished = jobs
        .values()
        .filter(|record| record.finished_instant.is_some())
        .count();
    if finished <= MAX_RETAINED {
        return;
    }
    let mut ages: Vec<(JobId, Instant)> = jobs
        .iter()
        .filter_map(|(id, record)| record.finished_instant.map(|at| (id.clone(), at)))
        .collect();
    // Oldest first, breaking ties by id so eviction is deterministic.
    ages.sort_by(|a, b| a.1.cmp(&b.1).then_with(|| a.0.as_str().cmp(b.0.as_str())));
    for (id, _) in ages.into_iter().take(finished - MAX_RETAINED) {
        jobs.remove(&id);
    }
}

/// A job registered as running. Exactly one of its methods records how the
/// job ended; dropping it without doing so removes the record, so a job the
/// engine never admitted leaves nothing behind.
#[derive(Debug)]
pub struct RunningJob<'a> {
    registry: &'a JobRegistry,
    id: JobId,
    settled: bool,
}

impl RunningJob<'_> {
    pub fn id(&self) -> &JobId {
        &self.id
    }

    /// The job produced its results.
    pub fn completed(mut self) {
        self.settled = true;
        self.registry.settle(&self.id, JobStatus::Completed, None);
    }

    /// The job ended without results. A cancellation is recorded as
    /// `CANCELLED`; anything else is a failure and keeps its diagnosis
    /// (SPECIFICATIONS.md section 94).
    pub fn ended(mut self, error: &Error) {
        self.settled = true;
        match error.kind() {
            Kind::Cancelled => {
                self.registry.settle(&self.id, JobStatus::Cancelled, None);
            }
            _ => {
                self.registry.settle(
                    &self.id,
                    JobStatus::Failed,
                    Some(Failure::from_error(error)),
                );
            }
        }
    }

    /// The job never started: the engine refused it. Nothing is recorded.
    pub fn never_started(mut self) {
        self.settled = true;
        self.registry.forget(&self.id);
    }
}

impl Drop for RunningJob<'_> {
    fn drop(&mut self) {
        if !self.settled {
            // A handler that returns early must not leave a job recorded as
            // running for ever.
            self.registry.forget(&self.id);
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    fn registry() -> JobRegistry {
        JobRegistry::new(Duration::from_secs(900))
    }

    fn id(name: &str) -> JobId {
        JobId::parse(name).unwrap()
    }

    fn version() -> AlgorithmVersion {
        AlgorithmVersion::new(1, 0, 0)
    }

    fn requested() -> Timestamp {
        Timestamp::parse("2026-09-06T12:00:00Z").unwrap()
    }

    #[test]
    fn a_running_job_is_visible_and_reports_how_long_it_has_run() {
        let registry = registry();
        let running = registry.start(&id("a"), version(), requested()).unwrap();

        let view = registry.get(&id("a")).expect("the job is running");
        assert_eq!(view.status, JobStatus::Processing);
        assert_eq!(view.requested_at, requested());
        assert_eq!(view.algorithm_version, version());
        assert!(view.running_for.is_some(), "a running job reports its age");
        assert!(view.finished_at.is_none());
        assert!(view.failure.is_none());

        running.completed();
        let view = registry.get(&id("a")).expect("the record is retained");
        assert_eq!(view.status, JobStatus::Completed);
        assert!(view.running_for.is_none(), "a finished job is not running");
        assert!(view.finished_at.is_some());
    }

    #[test]
    fn an_unknown_job_is_absent() {
        assert!(registry().get(&id("nope")).is_none());
    }

    #[test]
    fn a_second_run_of_the_same_id_conflicts_while_the_first_runs() {
        let registry = registry();
        let first = registry.start(&id("dup"), version(), requested()).unwrap();

        let error = registry
            .start(&id("dup"), version(), requested())
            .expect_err("the id is already running");
        assert_eq!(error.kind(), Kind::Conflict);
        assert_eq!(error.code(), "JOB_ALREADY_RUNNING");
        assert_eq!(
            registry.get(&id("dup")).unwrap().status,
            JobStatus::Processing,
            "the refused duplicate must not disturb the running record"
        );

        first.completed();
        // Once it has finished the id may be dispatched again.
        registry
            .start(&id("dup"), version(), requested())
            .expect("a finished id can run again")
            .completed();
    }

    #[test]
    fn a_failure_is_recorded_with_its_diagnosis_and_a_cancellation_is_not_a_failure() {
        let registry = registry();
        registry
            .start(&id("bad"), version(), requested())
            .unwrap()
            .ended(&Error::timeout());
        let view = registry.get(&id("bad")).unwrap();
        assert_eq!(view.status, JobStatus::Failed);
        let failure = view.failure.expect("a failed job keeps its diagnosis");
        assert_eq!(failure.code, "PROCESSING_TIMEOUT");
        assert!(failure.retryable);

        registry
            .start(&id("stopped"), version(), requested())
            .unwrap()
            .ended(&Error::cancelled());
        let view = registry.get(&id("stopped")).unwrap();
        assert_eq!(view.status, JobStatus::Cancelled);
        assert!(view.failure.is_none(), "a cancellation is not a failure");
    }

    #[test]
    fn a_job_the_engine_refused_leaves_no_record() {
        let registry = registry();
        registry
            .start(&id("refused"), version(), requested())
            .unwrap()
            .never_started();
        assert!(registry.get(&id("refused")).is_none());

        // The same happens when a handler drops the guard without settling.
        {
            let _guard = registry
                .start(&id("dropped"), version(), requested())
                .unwrap();
        }
        assert!(registry.get(&id("dropped")).is_none());
        assert!(registry.is_empty());
    }

    #[test]
    fn finished_records_expire_after_the_retention_window() {
        let registry = JobRegistry::new(Duration::ZERO);
        registry
            .start(&id("gone"), version(), requested())
            .unwrap()
            .completed();
        assert!(
            registry.get(&id("gone")).is_none(),
            "a zero retention window keeps nothing"
        );

        // A running job is never evicted, however short the window.
        let running = JobRegistry::new(Duration::ZERO);
        let _guard = running.start(&id("live"), version(), requested()).unwrap();
        assert!(running.get(&id("live")).is_some());
    }

    #[test]
    fn the_number_of_retained_records_is_bounded() {
        let registry = registry();
        for i in 0..(MAX_RETAINED + 50) {
            registry
                .start(&id(&format!("job-{i:05}")), version(), requested())
                .unwrap()
                .completed();
        }
        assert_eq!(
            registry.len(),
            MAX_RETAINED,
            "the registry must not grow past its bound"
        );
        assert!(
            registry
                .get(&id(&format!("job-{:05}", MAX_RETAINED + 49)))
                .is_some(),
            "the newest record survives"
        );
        assert!(
            registry.get(&id("job-00000")).is_none(),
            "the oldest record is evicted"
        );
    }
}
