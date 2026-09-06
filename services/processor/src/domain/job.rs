//! Processing jobs: status, parameters and the job's lifecycle invariants.

use std::collections::BTreeSet;
use std::time::Duration;

use serde::{Deserialize, Serialize};

use super::error::DomainError;
use super::ids::{JobId, PatientId};
use super::measurement::MeasurementType;
use super::timestamp::Timestamp;
use super::version::AlgorithmVersion;

/// Lifecycle state of a job, with the transitions allowed by the
/// specification's state machine.
#[derive(Debug, Clone, Copy, PartialEq, Eq, Hash, Serialize, Deserialize)]
#[serde(rename_all = "SCREAMING_SNAKE_CASE")]
pub enum JobStatus {
    Pending,
    Processing,
    Completed,
    Failed,
    Cancelled,
}

impl JobStatus {
    pub fn as_str(self) -> &'static str {
        match self {
            Self::Pending => "PENDING",
            Self::Processing => "PROCESSING",
            Self::Completed => "COMPLETED",
            Self::Failed => "FAILED",
            Self::Cancelled => "CANCELLED",
        }
    }

    /// Whether no further transition is possible.
    pub fn is_terminal(self) -> bool {
        matches!(self, Self::Completed | Self::Failed | Self::Cancelled)
    }

    /// Whether the state machine allows moving from `self` to `next`.
    pub fn can_transition_to(self, next: Self) -> bool {
        matches!(
            (self, next),
            (Self::Pending, Self::Processing)
                | (Self::Pending, Self::Cancelled)
                | (Self::Processing, Self::Completed)
                | (Self::Processing, Self::Failed)
                | (Self::Processing, Self::Cancelled)
        )
    }

    /// Returns `next` if the transition is allowed, otherwise a conflict error.
    pub fn transition(self, next: Self) -> Result<Self, DomainError> {
        if self.can_transition_to(next) {
            Ok(next)
        } else {
            Err(DomainError::InvalidTransition {
                from: self.as_str(),
                to: next.as_str(),
            })
        }
    }
}

/// Aggregation windows supported by the engine.
#[derive(Debug, Clone, Copy, PartialEq, Eq, Hash, PartialOrd, Ord, Serialize, Deserialize)]
pub enum Window {
    #[serde(rename = "1m")]
    OneMinute,
    #[serde(rename = "5m")]
    FiveMinutes,
    #[serde(rename = "15m")]
    FifteenMinutes,
    #[serde(rename = "1h")]
    OneHour,
    #[serde(rename = "6h")]
    SixHours,
    #[serde(rename = "24h")]
    OneDay,
    #[serde(rename = "7d")]
    SevenDays,
}

impl Window {
    pub const ALL: [Self; 7] = [
        Self::OneMinute,
        Self::FiveMinutes,
        Self::FifteenMinutes,
        Self::OneHour,
        Self::SixHours,
        Self::OneDay,
        Self::SevenDays,
    ];

    pub fn as_str(self) -> &'static str {
        match self {
            Self::OneMinute => "1m",
            Self::FiveMinutes => "5m",
            Self::FifteenMinutes => "15m",
            Self::OneHour => "1h",
            Self::SixHours => "6h",
            Self::OneDay => "24h",
            Self::SevenDays => "7d",
        }
    }

    pub fn duration(self) -> Duration {
        const MINUTE: u64 = 60;
        const HOUR: u64 = 60 * MINUTE;
        Duration::from_secs(match self {
            Self::OneMinute => MINUTE,
            Self::FiveMinutes => 5 * MINUTE,
            Self::FifteenMinutes => 15 * MINUTE,
            Self::OneHour => HOUR,
            Self::SixHours => 6 * HOUR,
            Self::OneDay => 24 * HOUR,
            Self::SevenDays => 7 * 24 * HOUR,
        })
    }
}

/// A percentile rank between 1 and 99 inclusive.
#[derive(Debug, Clone, Copy, PartialEq, Eq, Hash, PartialOrd, Ord, Serialize, Deserialize)]
#[serde(try_from = "u8", into = "u8")]
pub struct Percentile(u8);

impl Percentile {
    pub fn new(rank: u8) -> Result<Self, DomainError> {
        if (1..=99).contains(&rank) {
            Ok(Self(rank))
        } else {
            Err(DomainError::InvalidPercentile { value: rank })
        }
    }

    pub fn rank(self) -> u8 {
        self.0
    }
}

impl TryFrom<u8> for Percentile {
    type Error = DomainError;

    fn try_from(rank: u8) -> Result<Self, DomainError> {
        Self::new(rank)
    }
}

impl From<Percentile> for u8 {
    fn from(p: Percentile) -> Self {
        p.0
    }
}

/// What a job computes. Lists are de-duplicated and sorted so that equal
/// requests are represented identically.
#[derive(Debug, Clone, PartialEq, Eq, Serialize, Deserialize)]
#[serde(try_from = "RawParameters")]
pub struct ProcessingParameters {
    measurement_types: Vec<MeasurementType>,
    windows: Vec<Window>,
    percentiles: Vec<Percentile>,
}

impl ProcessingParameters {
    /// Requires at least one measurement type and one window; percentiles
    /// may be empty.
    pub fn new(
        measurement_types: impl IntoIterator<Item = MeasurementType>,
        windows: impl IntoIterator<Item = Window>,
        percentiles: impl IntoIterator<Item = Percentile>,
    ) -> Result<Self, DomainError> {
        let measurement_types: BTreeSet<_> = measurement_types.into_iter().collect();
        let windows: BTreeSet<_> = windows.into_iter().collect();
        let percentiles: BTreeSet<_> = percentiles.into_iter().collect();
        if measurement_types.is_empty() {
            return Err(DomainError::EmptyParameters {
                what: "measurement type",
            });
        }
        if windows.is_empty() {
            return Err(DomainError::EmptyParameters { what: "window" });
        }
        Ok(Self {
            measurement_types: measurement_types.into_iter().collect(),
            windows: windows.into_iter().collect(),
            percentiles: percentiles.into_iter().collect(),
        })
    }

    pub fn measurement_types(&self) -> &[MeasurementType] {
        &self.measurement_types
    }

    pub fn windows(&self) -> &[Window] {
        &self.windows
    }

    pub fn percentiles(&self) -> &[Percentile] {
        &self.percentiles
    }
}

#[derive(Debug, Deserialize)]
#[serde(deny_unknown_fields)]
struct RawParameters {
    measurement_types: Vec<MeasurementType>,
    windows: Vec<Window>,
    #[serde(default)]
    percentiles: Vec<Percentile>,
}

impl TryFrom<RawParameters> for ProcessingParameters {
    type Error = DomainError;

    fn try_from(raw: RawParameters) -> Result<Self, DomainError> {
        Self::new(raw.measurement_types, raw.windows, raw.percentiles)
    }
}

/// A processing job and its lifecycle. Timestamps are monotonic:
/// `requested_at <= started_at <= finished_at`, and the status determines
/// which of them are present.
#[derive(Debug, Clone, PartialEq, Eq, Serialize, Deserialize)]
#[serde(try_from = "RawProcessingJob")]
pub struct ProcessingJob {
    id: JobId,
    patient_id: PatientId,
    parameters: ProcessingParameters,
    algorithm_version: AlgorithmVersion,
    status: JobStatus,
    requested_at: Timestamp,
    started_at: Option<Timestamp>,
    finished_at: Option<Timestamp>,
}

impl ProcessingJob {
    /// A new job in the `PENDING` state.
    pub fn new(
        id: JobId,
        patient_id: PatientId,
        parameters: ProcessingParameters,
        algorithm_version: AlgorithmVersion,
        requested_at: Timestamp,
    ) -> Self {
        Self {
            id,
            patient_id,
            parameters,
            algorithm_version,
            status: JobStatus::Pending,
            requested_at,
            started_at: None,
            finished_at: None,
        }
    }

    pub fn id(&self) -> &JobId {
        &self.id
    }

    pub fn patient_id(&self) -> &PatientId {
        &self.patient_id
    }

    pub fn parameters(&self) -> &ProcessingParameters {
        &self.parameters
    }

    pub fn algorithm_version(&self) -> AlgorithmVersion {
        self.algorithm_version
    }

    pub fn status(&self) -> JobStatus {
        self.status
    }

    pub fn requested_at(&self) -> Timestamp {
        self.requested_at
    }

    pub fn started_at(&self) -> Option<Timestamp> {
        self.started_at
    }

    pub fn finished_at(&self) -> Option<Timestamp> {
        self.finished_at
    }

    /// `PENDING -> PROCESSING`.
    pub fn start(&mut self, now: Timestamp) -> Result<(), DomainError> {
        self.status.transition(JobStatus::Processing)?;
        if now < self.requested_at {
            return Err(DomainError::TimestampOrder {
                what: "started_at is earlier than requested_at",
            });
        }
        self.status = JobStatus::Processing;
        self.started_at = Some(now);
        Ok(())
    }

    /// `PROCESSING -> COMPLETED`.
    pub fn complete(&mut self, now: Timestamp) -> Result<(), DomainError> {
        self.finish(JobStatus::Completed, now)
    }

    /// `PROCESSING -> FAILED`.
    pub fn fail(&mut self, now: Timestamp) -> Result<(), DomainError> {
        self.finish(JobStatus::Failed, now)
    }

    /// `PENDING -> CANCELLED` or `PROCESSING -> CANCELLED`.
    pub fn cancel(&mut self, now: Timestamp) -> Result<(), DomainError> {
        self.finish(JobStatus::Cancelled, now)
    }

    fn finish(&mut self, next: JobStatus, now: Timestamp) -> Result<(), DomainError> {
        self.status.transition(next)?;
        let after = self.started_at.unwrap_or(self.requested_at);
        if now < after {
            return Err(DomainError::TimestampOrder {
                what: "finished_at is earlier than the previous lifecycle timestamp",
            });
        }
        self.status = next;
        self.finished_at = Some(now);
        Ok(())
    }

    fn check_consistency(&self) -> Result<(), DomainError> {
        let reject = |reason| Err(DomainError::InconsistentJob { reason });
        match self.status {
            JobStatus::Pending => {
                if self.started_at.is_some() || self.finished_at.is_some() {
                    return reject("a PENDING job has no started_at or finished_at");
                }
            }
            JobStatus::Processing => {
                if self.started_at.is_none() {
                    return reject("a PROCESSING job requires started_at");
                }
                if self.finished_at.is_some() {
                    return reject("a PROCESSING job has no finished_at");
                }
            }
            JobStatus::Completed | JobStatus::Failed => {
                if self.started_at.is_none() {
                    return reject("a COMPLETED or FAILED job requires started_at");
                }
                if self.finished_at.is_none() {
                    return reject("a COMPLETED or FAILED job requires finished_at");
                }
            }
            JobStatus::Cancelled => {
                if self.finished_at.is_none() {
                    return reject("a CANCELLED job requires finished_at");
                }
            }
        }
        if self.started_at.is_some_and(|s| s < self.requested_at) {
            return Err(DomainError::TimestampOrder {
                what: "started_at is earlier than requested_at",
            });
        }
        let after = self.started_at.unwrap_or(self.requested_at);
        if self.finished_at.is_some_and(|f| f < after) {
            return Err(DomainError::TimestampOrder {
                what: "finished_at is earlier than the previous lifecycle timestamp",
            });
        }
        Ok(())
    }
}

#[derive(Debug, Deserialize)]
#[serde(deny_unknown_fields)]
struct RawProcessingJob {
    id: JobId,
    patient_id: PatientId,
    parameters: ProcessingParameters,
    algorithm_version: AlgorithmVersion,
    status: JobStatus,
    requested_at: Timestamp,
    #[serde(default)]
    started_at: Option<Timestamp>,
    #[serde(default)]
    finished_at: Option<Timestamp>,
}

impl TryFrom<RawProcessingJob> for ProcessingJob {
    type Error = DomainError;

    fn try_from(raw: RawProcessingJob) -> Result<Self, DomainError> {
        let job = Self {
            id: raw.id,
            patient_id: raw.patient_id,
            parameters: raw.parameters,
            algorithm_version: raw.algorithm_version,
            status: raw.status,
            requested_at: raw.requested_at,
            started_at: raw.started_at,
            finished_at: raw.finished_at,
        };
        job.check_consistency()?;
        Ok(job)
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use JobStatus::*;

    fn ts(raw: &str) -> Timestamp {
        Timestamp::parse(raw).unwrap()
    }

    fn params() -> ProcessingParameters {
        ProcessingParameters::new(
            [MeasurementType::HeartRate],
            [Window::OneHour],
            [Percentile::new(95).unwrap()],
        )
        .unwrap()
    }

    fn job() -> ProcessingJob {
        ProcessingJob::new(
            JobId::parse("job-1").unwrap(),
            PatientId::parse("p-1").unwrap(),
            params(),
            AlgorithmVersion::new(1, 0, 0),
            ts("2026-09-06T12:00:00Z"),
        )
    }

    #[test]
    fn only_specified_transitions_are_allowed() {
        let all = [Pending, Processing, Completed, Failed, Cancelled];
        let allowed = [
            (Pending, Processing),
            (Pending, Cancelled),
            (Processing, Completed),
            (Processing, Failed),
            (Processing, Cancelled),
        ];
        for from in all {
            for to in all {
                let want = allowed.contains(&(from, to));
                assert_eq!(from.can_transition_to(to), want, "{from:?} -> {to:?}");
                match from.transition(to) {
                    Ok(next) => assert!(want && next == to),
                    Err(err) => {
                        assert!(!want);
                        assert_eq!(err.code(), "INVALID_JOB_TRANSITION");
                    }
                }
            }
        }
        assert!(Completed.is_terminal() && Failed.is_terminal() && Cancelled.is_terminal());
        assert!(!Pending.is_terminal() && !Processing.is_terminal());
    }

    #[test]
    fn status_serde_and_invalid_variants() {
        assert_eq!(
            serde_json::to_string(&Processing).unwrap(),
            "\"PROCESSING\""
        );
        assert_eq!(
            serde_json::from_str::<JobStatus>("\"CANCELLED\"").unwrap(),
            Cancelled
        );
        for bad in ["\"processing\"", "\"DONE\"", "1"] {
            assert!(serde_json::from_str::<JobStatus>(bad).is_err(), "{bad}");
        }
    }

    #[test]
    fn windows_have_durations_and_wire_names() {
        assert_eq!(Window::OneMinute.duration(), Duration::from_secs(60));
        assert_eq!(
            Window::SevenDays.duration(),
            Duration::from_secs(7 * 86_400)
        );
        for window in Window::ALL {
            let json = serde_json::to_string(&window).unwrap();
            assert_eq!(json, format!("\"{}\"", window.as_str()));
            assert_eq!(serde_json::from_str::<Window>(&json).unwrap(), window);
        }
        let sorted = Window::ALL;
        assert!(
            sorted.windows(2).all(|w| w[0].duration() < w[1].duration()),
            "ALL is ascending"
        );
        for bad in ["\"1d\"", "\"60s\"", "\"1M\"", "\"\""] {
            assert!(serde_json::from_str::<Window>(bad).is_err(), "{bad}");
        }
    }

    #[test]
    fn percentiles_are_between_1_and_99() {
        assert_eq!(Percentile::new(1).unwrap().rank(), 1);
        assert_eq!(Percentile::new(99).unwrap().rank(), 99);
        for bad in [0, 100, 255] {
            assert_eq!(
                Percentile::new(bad).unwrap_err(),
                DomainError::InvalidPercentile { value: bad }
            );
        }
        assert_eq!(
            serde_json::to_string(&Percentile::new(95).unwrap()).unwrap(),
            "95"
        );
        assert_eq!(serde_json::from_str::<Percentile>("50").unwrap().rank(), 50);
        for bad in ["0", "100", "-5", "50.5", "\"50\"", "256"] {
            assert!(serde_json::from_str::<Percentile>(bad).is_err(), "{bad}");
        }
    }

    #[test]
    fn parameters_are_deduplicated_sorted_and_non_empty() {
        let p = ProcessingParameters::new(
            [
                MeasurementType::Spo2,
                MeasurementType::HeartRate,
                MeasurementType::Spo2,
            ],
            [Window::SevenDays, Window::OneMinute, Window::OneMinute],
            [
                Percentile::new(99).unwrap(),
                Percentile::new(50).unwrap(),
                Percentile::new(99).unwrap(),
            ],
        )
        .unwrap();
        assert_eq!(
            p.measurement_types(),
            [MeasurementType::HeartRate, MeasurementType::Spo2]
        );
        assert_eq!(p.windows(), [Window::OneMinute, Window::SevenDays]);
        assert_eq!(
            p.percentiles().iter().map(|p| p.rank()).collect::<Vec<_>>(),
            [50, 99]
        );

        assert_eq!(
            ProcessingParameters::new([], [Window::OneHour], []).unwrap_err(),
            DomainError::EmptyParameters {
                what: "measurement type"
            }
        );
        assert_eq!(
            ProcessingParameters::new([MeasurementType::Spo2], [], []).unwrap_err(),
            DomainError::EmptyParameters { what: "window" }
        );
    }

    #[test]
    fn parameters_serde() {
        let json = r#"{"measurement_types":["SPO2","HEART_RATE"],"windows":["1h","1m"]}"#;
        let p: ProcessingParameters = serde_json::from_str(json).unwrap();
        assert!(p.percentiles().is_empty(), "percentiles default to empty");
        assert_eq!(
            serde_json::to_string(&p).unwrap(),
            r#"{"measurement_types":["HEART_RATE","SPO2"],"windows":["1m","1h"],"percentiles":[]}"#
        );
        for bad in [
            r#"{"measurement_types":[],"windows":["1h"]}"#,
            r#"{"measurement_types":["SPO2"],"windows":[]}"#,
            r#"{"measurement_types":["SPO2"],"windows":["1h"],"percentiles":[0]}"#,
            r#"{"measurement_types":["SPO2"],"windows":["1h"],"extra":true}"#,
            r#"{"windows":["1h"]}"#,
        ] {
            assert!(
                serde_json::from_str::<ProcessingParameters>(bad).is_err(),
                "{bad}"
            );
        }
    }

    #[test]
    fn lifecycle_records_monotonic_timestamps() {
        let mut j = job();
        assert_eq!(j.status(), Pending);
        assert_eq!((j.started_at(), j.finished_at()), (None, None));

        assert_eq!(
            j.complete(ts("2026-09-06T12:00:01Z")).unwrap_err().code(),
            "INVALID_JOB_TRANSITION"
        );
        assert_eq!(
            j.start(ts("2026-09-06T11:59:59Z")).unwrap_err(),
            DomainError::TimestampOrder {
                what: "started_at is earlier than requested_at"
            }
        );
        assert_eq!(j.status(), Pending, "a rejected transition changes nothing");

        j.start(ts("2026-09-06T12:00:00Z")).unwrap();
        assert_eq!(j.status(), Processing);
        assert_eq!(j.started_at(), Some(ts("2026-09-06T12:00:00Z")));

        assert!(
            j.complete(ts("2026-09-06T11:59:00Z")).is_err(),
            "finish before start"
        );
        j.complete(ts("2026-09-06T12:05:00Z")).unwrap();
        assert_eq!(j.status(), Completed);
        assert_eq!(j.finished_at(), Some(ts("2026-09-06T12:05:00Z")));
        assert!(j.cancel(ts("2026-09-06T12:06:00Z")).is_err(), "terminal");

        let mut cancelled = job();
        cancelled.cancel(ts("2026-09-06T12:00:00Z")).unwrap();
        assert_eq!(
            (cancelled.status(), cancelled.started_at()),
            (Cancelled, None)
        );

        let mut failed = job();
        failed.start(ts("2026-09-06T12:00:00Z")).unwrap();
        failed.fail(ts("2026-09-06T12:00:00Z")).unwrap();
        assert_eq!(failed.status(), Failed);
    }

    #[test]
    fn serde_round_trip_and_consistency_checks() {
        let mut j = job();
        j.start(ts("2026-09-06T12:00:01Z")).unwrap();
        let json = serde_json::to_string(&j).unwrap();
        assert!(json.contains(r#""status":"PROCESSING""#));
        assert!(json.contains(r#""finished_at":null"#));
        assert_eq!(serde_json::from_str::<ProcessingJob>(&json).unwrap(), j);

        let base = r#""id":"job-1","patient_id":"p-1","parameters":{"measurement_types":["SPO2"],"windows":["1h"]},"algorithm_version":"1.0.0","requested_at":"2026-09-06T12:00:00Z""#;
        let ok = [
            format!(r#"{{{base},"status":"PENDING"}}"#),
            format!(r#"{{{base},"status":"CANCELLED","finished_at":"2026-09-06T12:00:00Z"}}"#),
            format!(
                r#"{{{base},"status":"FAILED","started_at":"2026-09-06T12:00:00Z","finished_at":"2026-09-06T12:00:00Z"}}"#
            ),
        ];
        for json in &ok {
            serde_json::from_str::<ProcessingJob>(json).unwrap_or_else(|e| panic!("{json}: {e}"));
        }

        let bad = [
            (
                format!(r#"{{{base},"status":"PENDING","started_at":"2026-09-06T12:00:00Z"}}"#),
                "PENDING",
            ),
            (
                format!(r#"{{{base},"status":"PROCESSING"}}"#),
                "requires started_at",
            ),
            (
                format!(r#"{{{base},"status":"COMPLETED","started_at":"2026-09-06T12:00:00Z"}}"#),
                "requires finished_at",
            ),
            (
                format!(r#"{{{base},"status":"CANCELLED"}}"#),
                "requires finished_at",
            ),
            (
                format!(r#"{{{base},"status":"PROCESSING","started_at":"2026-09-06T11:00:00Z"}}"#),
                "earlier than requested_at",
            ),
            (
                format!(
                    r#"{{{base},"status":"COMPLETED","started_at":"2026-09-06T12:00:00Z","finished_at":"2026-09-06T11:00:00Z"}}"#
                ),
                "finished_at is earlier",
            ),
            (
                format!(r#"{{{base},"status":"RUNNING"}}"#),
                "unknown variant",
            ),
            (
                format!(r#"{{{base},"status":"PENDING","note":"x"}}"#),
                "unknown field",
            ),
        ];
        for (json, want) in &bad {
            let err = serde_json::from_str::<ProcessingJob>(json).expect_err(json);
            assert!(
                err.to_string().contains(want),
                "{json}\n  got: {err}\n  want: {want}"
            );
        }
    }
}
