//! The processing pipeline (SPECIFICATIONS.md section 14):
//!
//! ```text
//! Raw Measurements -> Validation -> Normalization -> Time Ordering
//!   -> Window Aggregation -> Statistical Analysis -> Anomaly Detection
//!   -> Result Generation
//! ```
//!
//! [`Pipeline::run`] is the synchronous, deterministic core: given a job,
//! raw readings and a [`Control`], it returns an [`Outcome`] or an error.
//! [`Processor::process`] runs that core under the execution
//! [`Engine`]: bounded concurrency, a time bound, per-job and shutdown
//! cancellation. The core checks the control at every stage boundary and
//! every few windows, so a cancelled or timed-out job stops within a
//! bounded amount of work rather than running to completion in the
//! background.
//!
//! # Guarantees
//!
//! - **Bounded memory.** A job is refused before parsing when it carries
//!   more than `max_job_measurements` readings; everything else the
//!   pipeline holds is proportional to that bound.
//! - **Bounded concurrency.** At most `MAX_CONCURRENT_JOBS` cores run at
//!   once; each runs on tokio's blocking pool, one task per admitted job,
//!   never more.
//! - **Determinism.** Validation uses the job's `requested_at` as the
//!   reference clock, ordering is total, and every stage is a fixed
//!   sequence of operations, so equal input yields equal output.
//! - **Explicit versioning.** A job must request the engine's
//!   [`ALGORITHM_VERSION`]; every result carries it and the service version.
//! - **Partial failure reporting.** Readings the validation stage rejects
//!   are reported by index with a stable code; the rest are processed. A
//!   job with no valid reading fails as a whole.
//! - **Safe errors.** Every failure is an [`Error`] with a kind and stable
//!   code; internal causes are attached as sources, never as messages.

mod stages;

use std::num::NonZeroUsize;
use std::sync::Arc;
use std::time::{Duration, Instant};

use serde::{Deserialize, Serialize};
use tokio_util::sync::CancellationToken;

use crate::anomaly::{ALGORITHM_VERSION, AnomalyError, Detector, run_with as detect};
use crate::domain::{
    AlgorithmVersion, JobId, Measurement, ProcessingJob, ProcessingResult, ProcessingResultInput,
    TimestampBounds,
};
use crate::engine::Engine;
use crate::error::{Error, Kind, Result};

pub use stages::{Rejection, RejectionCode};

/// One reading as received, before validation. Every field is a plain
/// string or number so that a malformed reading is reported individually
/// instead of failing the whole request at parse time.
#[derive(Debug, Clone, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct RawReading {
    pub id: String,
    #[serde(rename = "type")]
    pub measurement_type: String,
    pub value: f64,
    pub unit: String,
    pub recorded_at: String,
}

/// A processing request: the job and the readings to process.
#[derive(Debug, Clone, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct Request {
    pub job: ProcessingJob,
    pub readings: Vec<RawReading>,
}

/// What a job produced.
#[derive(Debug, Clone, PartialEq, Eq, Serialize)]
pub struct Outcome {
    pub job_id: JobId,
    pub algorithm_version: AlgorithmVersion,
    pub service_version: String,
    /// Readings that passed validation and normalisation.
    pub accepted: usize,
    /// Valid readings of types the job did not request.
    pub skipped: usize,
    /// Readings rejected by validation or normalisation, by input index.
    pub rejected: Vec<Rejection>,
    pub results: Vec<ProcessingResult>,
}

/// Bounds applied to every job.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub struct Limits {
    /// Largest number of readings one job may carry.
    pub max_job_measurements: NonZeroUsize,
    /// Acceptance window for `recorded_at`, relative to the job's
    /// `requested_at`.
    pub timestamp_bounds: TimestampBounds,
}

/// Cooperative cancellation and deadline for one run of the core.
#[derive(Debug, Clone)]
pub struct Control {
    cancel: CancellationToken,
    deadline: Option<Instant>,
}

impl Control {
    pub fn new(cancel: CancellationToken, deadline: Option<Instant>) -> Self {
        Self { cancel, deadline }
    }

    /// A control that never stops the run.
    pub fn unbounded() -> Self {
        Self::new(CancellationToken::new(), None)
    }

    /// Fails when the run has been cancelled or its deadline has passed.
    pub fn check(&self) -> Result<()> {
        if self.cancel.is_cancelled() {
            return Err(Error::cancelled());
        }
        if self.deadline.is_some_and(|d| Instant::now() >= d) {
            return Err(Error::timeout());
        }
        Ok(())
    }
}

/// Windows processed between two control checks.
const CHECK_EVERY_WINDOWS: usize = 64;

/// The deterministic core.
#[derive(Debug, Clone)]
pub struct Pipeline {
    detector: Detector,
    limits: Limits,
    service_version: String,
}

impl Pipeline {
    pub fn new(detector: Detector, limits: Limits, service_version: impl Into<String>) -> Self {
        Self {
            detector,
            limits,
            service_version: service_version.into(),
        }
    }

    pub fn limits(&self) -> Limits {
        self.limits
    }

    pub fn algorithm_version(&self) -> AlgorithmVersion {
        ALGORITHM_VERSION
    }

    /// Runs every stage over `request`, checking `control` between them.
    pub fn run(&self, request: Request, control: &Control) -> Result<Outcome> {
        let job = request.job;
        if job.algorithm_version() != ALGORITHM_VERSION {
            return Err(Error::new(
                Kind::Validation,
                "UNSUPPORTED_ALGORITHM_VERSION",
                format!(
                    "the job requests algorithm version {} but this processor implements {}",
                    job.algorithm_version(),
                    ALGORITHM_VERSION
                ),
            ));
        }
        let total = request.readings.len();
        if total > self.limits.max_job_measurements.get() {
            let maximum = self.limits.max_job_measurements.get();
            return Err(Error::new(
                Kind::Validation,
                "JOB_TOO_LARGE",
                format!("the job carries {total} readings; the maximum is {maximum}"),
            )
            // The limit is configuration, not a secret, and a client that
            // knows it can split the job without parsing the message.
            .with_details([
                ("max_job_measurements", maximum as i64),
                ("received", total as i64),
            ]));
        }
        control.check()?;

        // Validation consumes the readings: accepted identifiers keep their
        // allocation and the raw strings are freed as they are checked.
        let (validated, mut rejected) = stages::validate(
            request.readings,
            &self.limits.timestamp_bounds,
            job.requested_at(),
        );
        control.check()?;

        // Normalization: duplicate identifiers, requested types only.
        let (mut measurements, skipped) =
            stages::normalize(validated, job.parameters(), &mut rejected);
        rejected.sort_by_key(|r| r.index);
        if measurements.is_empty() {
            return Err(Error::new(
                Kind::Validation,
                "NO_VALID_MEASUREMENTS",
                format!(
                    "none of the {} readings can be processed ({} rejected, {} of unrequested types)",
                    total,
                    rejected.len(),
                    skipped
                ),
            ));
        }
        let accepted = measurements.len();
        control.check()?;

        // Time ordering, window aggregation, statistics and anomaly
        // detection, checking the control every few windows.
        let reports = self.analyse(&mut measurements, &job, control)?;

        // Result generation.
        let mut results = Vec::with_capacity(reports.len());
        for report in reports {
            let result = ProcessingResult::new(ProcessingResultInput {
                job_id: job.id().clone(),
                measurement_type: report.measurement_type,
                window: report.window,
                window_start: report.window_start,
                statistics: report.statistics,
                anomalies: report.anomalies,
                algorithm_version: ALGORITHM_VERSION,
                service_version: self.service_version.clone(),
            })
            .map_err(Error::internal)?;
            results.push(result);
        }
        Ok(Outcome {
            job_id: job.id().clone(),
            algorithm_version: ALGORITHM_VERSION,
            service_version: self.service_version.clone(),
            accepted,
            skipped,
            rejected,
            results,
        })
    }

    fn analyse(
        &self,
        measurements: &mut [Measurement],
        job: &ProcessingJob,
        control: &Control,
    ) -> Result<Vec<crate::anomaly::WindowReport>> {
        // The anomaly pipeline orders, windows, summarises and detects in
        // one pass per type and window; it checks the control through the
        // callback every CHECK_EVERY_WINDOWS windows.
        let mut seen = 0usize;
        let mut check = || -> Result<()> {
            seen += 1;
            if seen % CHECK_EVERY_WINDOWS == 0 {
                control.check()?;
            }
            Ok(())
        };
        detect(measurements, job.parameters(), &self.detector, &mut check).map_err(|e| match e {
            AnomalyError::Interrupted(err) => err,
            other => Error::internal(other),
        })
    }
}

/// Runs pipeline cores under the execution engine.
#[derive(Debug, Clone)]
pub struct Processor {
    pipeline: Arc<Pipeline>,
    engine: Arc<Engine>,
}

impl Processor {
    pub fn new(pipeline: Pipeline, engine: Arc<Engine>) -> Self {
        Self {
            pipeline: Arc::new(pipeline),
            engine,
        }
    }

    pub fn pipeline(&self) -> &Pipeline {
        &self.pipeline
    }

    /// The engine as a shared handle, for a collector that must outlive
    /// the borrow.
    pub fn engine_handle(&self) -> Arc<Engine> {
        Arc::clone(&self.engine)
    }

    pub fn engine(&self) -> &Engine {
        &self.engine
    }

    /// Processes `request` as its job under the engine's limits. The core
    /// runs on the blocking pool as one task per admitted job; the engine
    /// bounds how many are admitted. Cancellation and the time bound reach
    /// the core through its [`Control`], so a stopped job releases its
    /// thread at the next checkpoint.
    pub async fn process(&self, request: Request) -> Result<Outcome> {
        self.process_within(request, None).await
    }

    /// [`Self::process`] bounded by the smaller of `budget` and the engine's
    /// own `PROCESSING_TIMEOUT`.
    ///
    /// A client declares `budget` when it has less time left than the
    /// processor would take; honouring it means the core stops at its next
    /// checkpoint instead of finishing work nobody is waiting for. It can
    /// only shorten the bound: a client cannot buy itself more time than the
    /// processor allows.
    pub async fn process_within(
        &self,
        request: Request,
        budget: Option<Duration>,
    ) -> Result<Outcome> {
        let id = request.job.id().clone();
        let cancel = CancellationToken::new();
        let bound = budget
            .unwrap_or(self.engine.timeout())
            .min(self.engine.timeout());
        let deadline = Instant::now() + bound;
        let control = Control::new(cancel.clone(), Some(deadline));
        let pipeline = Arc::clone(&self.pipeline);

        let work = async move {
            let handle = tokio::task::spawn_blocking(move || pipeline.run(request, &control));
            match handle.await {
                Ok(outcome) => outcome,
                Err(join) if join.is_cancelled() => Err(Error::cancelled()),
                Err(join) => Err(Error::internal(join)),
            }
        };
        let _stop_core_when_dropped = StopOnDrop(cancel);
        self.engine.run(id, work).await
    }
}

/// Cancels the core's control when the surrounding future ends for any
/// reason, so a job the engine abandoned (timeout, cancellation, shutdown)
/// stops at its next checkpoint instead of running to completion.
struct StopOnDrop(CancellationToken);

impl Drop for StopOnDrop {
    fn drop(&mut self) {
        self.0.cancel();
    }
}
