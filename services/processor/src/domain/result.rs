//! Processing results: per-window statistics, anomalies and severity.
//! The algorithms that produce them live elsewhere; this module only
//! defines what a valid result looks like.

use std::collections::BTreeMap;

use serde::{Deserialize, Serialize};

use super::error::DomainError;
use super::ids::JobId;
use super::job::{Percentile, Window};
use super::measurement::MeasurementType;
use super::timestamp::Timestamp;
use super::value::Finite;
use super::version::AlgorithmVersion;

/// Technical severity of an anomaly. It classifies how far a value departs
/// from expectation and carries no medical meaning.
#[derive(Debug, Clone, Copy, PartialEq, Eq, Hash, PartialOrd, Ord, Serialize, Deserialize)]
#[serde(rename_all = "SCREAMING_SNAKE_CASE")]
pub enum Severity {
    Info,
    Warning,
    Critical,
}

/// The rule that flagged an anomaly.
#[derive(Debug, Clone, Copy, PartialEq, Eq, Hash, Serialize, Deserialize)]
#[serde(rename_all = "SCREAMING_SNAKE_CASE")]
pub enum AnomalyMetric {
    Threshold,
    ZScore,
    RollingDeviation,
}

/// One flagged value.
#[derive(Debug, Clone, PartialEq, Eq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct Anomaly {
    pub measurement_type: MeasurementType,
    pub window: Window,
    pub metric: AnomalyMetric,
    pub value: Finite,
    pub threshold: Finite,
    pub severity: Severity,
    pub detected_at: Timestamp,
    pub algorithm_version: AlgorithmVersion,
}

/// The values of a [`Statistics`] before validation. This is also the wire
/// shape.
#[derive(Debug, Clone, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct StatisticsInput {
    pub count: u64,
    pub min: Finite,
    pub max: Finite,
    pub mean: Finite,
    pub median: Finite,
    pub variance: Finite,
    pub std_dev: Finite,
    #[serde(default)]
    pub percentiles: BTreeMap<Percentile, Finite>,
}

/// Descriptive statistics of the values in one window. Construction checks
/// the relations that must hold for any sample: `count >= 1`,
/// `min <= mean, median, percentiles <= max`, and non-negative spread.
#[derive(Debug, Clone, PartialEq, Eq, Serialize, Deserialize)]
#[serde(try_from = "StatisticsInput")]
pub struct Statistics {
    count: u64,
    min: Finite,
    max: Finite,
    mean: Finite,
    median: Finite,
    variance: Finite,
    std_dev: Finite,
    percentiles: BTreeMap<Percentile, Finite>,
}

impl Statistics {
    pub fn new(input: StatisticsInput) -> Result<Self, DomainError> {
        let reject = |reason| Err(DomainError::InconsistentStatistics { reason });
        let StatisticsInput {
            count,
            min,
            max,
            mean,
            median,
            variance,
            std_dev,
            percentiles,
        } = input;
        if count == 0 {
            return reject("count must be at least 1");
        }
        if min > max {
            return reject("min is greater than max");
        }
        if mean < min || mean > max {
            return reject("mean is outside [min, max]");
        }
        if median < min || median > max {
            return reject("median is outside [min, max]");
        }
        if variance < Finite::ZERO {
            return reject("variance is negative");
        }
        if std_dev < Finite::ZERO {
            return reject("std_dev is negative");
        }
        if percentiles.values().any(|v| *v < min || *v > max) {
            return reject("a percentile is outside [min, max]");
        }
        Ok(Self {
            count,
            min,
            max,
            mean,
            median,
            variance,
            std_dev,
            percentiles,
        })
    }

    pub fn count(&self) -> u64 {
        self.count
    }

    pub fn min(&self) -> Finite {
        self.min
    }

    pub fn max(&self) -> Finite {
        self.max
    }

    pub fn mean(&self) -> Finite {
        self.mean
    }

    pub fn median(&self) -> Finite {
        self.median
    }

    pub fn variance(&self) -> Finite {
        self.variance
    }

    pub fn std_dev(&self) -> Finite {
        self.std_dev
    }

    pub fn percentiles(&self) -> &BTreeMap<Percentile, Finite> {
        &self.percentiles
    }
}

impl TryFrom<StatisticsInput> for Statistics {
    type Error = DomainError;

    fn try_from(input: StatisticsInput) -> Result<Self, DomainError> {
        Self::new(input)
    }
}

/// The values of a [`ProcessingResult`] before validation. This is also the
/// wire shape.
#[derive(Debug, Clone, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct ProcessingResultInput {
    pub job_id: JobId,
    pub measurement_type: MeasurementType,
    pub window: Window,
    pub window_start: Timestamp,
    pub statistics: Statistics,
    #[serde(default)]
    pub anomalies: Vec<Anomaly>,
    pub algorithm_version: AlgorithmVersion,
    pub service_version: String,
}

/// The result for one job, measurement type and window instance. Every
/// anomaly refers to the same type, window and algorithm version and was
/// detected inside the window.
#[derive(Debug, Clone, PartialEq, Eq, Serialize, Deserialize)]
#[serde(try_from = "ProcessingResultInput")]
pub struct ProcessingResult {
    job_id: JobId,
    measurement_type: MeasurementType,
    window: Window,
    window_start: Timestamp,
    statistics: Statistics,
    anomalies: Vec<Anomaly>,
    algorithm_version: AlgorithmVersion,
    service_version: String,
}

impl ProcessingResult {
    pub fn new(input: ProcessingResultInput) -> Result<Self, DomainError> {
        let reject = |reason| Err(DomainError::InconsistentResult { reason });
        if input.service_version.trim().is_empty() {
            return reject("service_version must not be empty");
        }
        let window_end = input.window_start.checked_add(input.window.duration());
        for anomaly in &input.anomalies {
            if anomaly.measurement_type != input.measurement_type {
                return reject("an anomaly has a different measurement type than the result");
            }
            if anomaly.window != input.window {
                return reject("an anomaly has a different window than the result");
            }
            if anomaly.algorithm_version != input.algorithm_version {
                return reject("an anomaly has a different algorithm version than the result");
            }
            let inside = anomaly.detected_at >= input.window_start
                && window_end.is_none_or(|end| anomaly.detected_at < end);
            if !inside {
                return reject("an anomaly was detected outside the result's window");
            }
        }
        Ok(Self {
            job_id: input.job_id,
            measurement_type: input.measurement_type,
            window: input.window,
            window_start: input.window_start,
            statistics: input.statistics,
            anomalies: input.anomalies,
            algorithm_version: input.algorithm_version,
            service_version: input.service_version,
        })
    }

    pub fn job_id(&self) -> &JobId {
        &self.job_id
    }

    pub fn measurement_type(&self) -> MeasurementType {
        self.measurement_type
    }

    pub fn window(&self) -> Window {
        self.window
    }

    pub fn window_start(&self) -> Timestamp {
        self.window_start
    }

    /// Exclusive end of the window, if representable.
    pub fn window_end(&self) -> Option<Timestamp> {
        self.window_start.checked_add(self.window.duration())
    }

    pub fn statistics(&self) -> &Statistics {
        &self.statistics
    }

    pub fn anomalies(&self) -> &[Anomaly] {
        &self.anomalies
    }

    pub fn algorithm_version(&self) -> AlgorithmVersion {
        self.algorithm_version
    }

    pub fn service_version(&self) -> &str {
        &self.service_version
    }
}

impl TryFrom<ProcessingResultInput> for ProcessingResult {
    type Error = DomainError;

    fn try_from(input: ProcessingResultInput) -> Result<Self, DomainError> {
        Self::new(input)
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    fn f(v: f64) -> Finite {
        Finite::new(v).unwrap()
    }

    fn ts(raw: &str) -> Timestamp {
        Timestamp::parse(raw).unwrap()
    }

    fn stats_input() -> StatisticsInput {
        let mut percentiles = BTreeMap::new();
        percentiles.insert(Percentile::new(50).unwrap(), f(70.0));
        percentiles.insert(Percentile::new(95).unwrap(), f(88.0));
        StatisticsInput {
            count: 10,
            min: f(60.0),
            max: f(90.0),
            mean: f(71.5),
            median: f(70.0),
            variance: f(64.0),
            std_dev: f(8.0),
            percentiles,
        }
    }

    fn stats() -> Statistics {
        Statistics::new(stats_input()).unwrap()
    }

    fn anomaly(at: &str) -> Anomaly {
        Anomaly {
            measurement_type: MeasurementType::HeartRate,
            window: Window::OneHour,
            metric: AnomalyMetric::ZScore,
            value: f(90.0),
            threshold: f(3.0),
            severity: Severity::Warning,
            detected_at: ts(at),
            algorithm_version: AlgorithmVersion::new(1, 0, 0),
        }
    }

    fn result_input(anomalies: Vec<Anomaly>) -> ProcessingResultInput {
        ProcessingResultInput {
            job_id: JobId::parse("job-1").unwrap(),
            measurement_type: MeasurementType::HeartRate,
            window: Window::OneHour,
            window_start: ts("2026-09-06T12:00:00Z"),
            statistics: stats(),
            anomalies,
            algorithm_version: AlgorithmVersion::new(1, 0, 0),
            service_version: "abc123".to_owned(),
        }
    }

    fn result(anomalies: Vec<Anomaly>) -> Result<ProcessingResult, DomainError> {
        ProcessingResult::new(result_input(anomalies))
    }

    #[test]
    fn severity_is_ordered_and_serialised_in_upper_case() {
        assert!(Severity::Info < Severity::Warning && Severity::Warning < Severity::Critical);
        assert_eq!(
            serde_json::to_string(&Severity::Critical).unwrap(),
            "\"CRITICAL\""
        );
        assert_eq!(
            serde_json::from_str::<Severity>("\"INFO\"").unwrap(),
            Severity::Info
        );
        for bad in ["\"info\"", "\"HIGH\"", "2"] {
            assert!(serde_json::from_str::<Severity>(bad).is_err(), "{bad}");
        }
        assert_eq!(
            serde_json::to_string(&AnomalyMetric::RollingDeviation).unwrap(),
            "\"ROLLING_DEVIATION\""
        );
        assert_eq!(
            serde_json::from_str::<AnomalyMetric>("\"Z_SCORE\"").unwrap(),
            AnomalyMetric::ZScore
        );
        assert!(serde_json::from_str::<AnomalyMetric>("\"MEAN\"").is_err());
    }

    #[test]
    fn statistics_boundaries_are_inclusive() {
        let single = Statistics::new(StatisticsInput {
            count: 1,
            min: f(5.0),
            max: f(5.0),
            mean: f(5.0),
            median: f(5.0),
            variance: f(0.0),
            std_dev: f(0.0),
            percentiles: BTreeMap::new(),
        })
        .unwrap();
        assert_eq!(single.count(), 1);
        assert_eq!(single.min(), single.max());
        let s = stats();
        assert_eq!(
            (s.mean(), s.median(), s.variance(), s.std_dev()),
            (f(71.5), f(70.0), f(64.0), f(8.0))
        );
        assert_eq!(s.percentiles().len(), 2);
    }

    #[test]
    fn statistics_reject_impossible_relations() {
        let make = |count: u64, values: [f64; 6], percentile: Option<f64>| {
            let [min, max, mean, median, variance, std_dev] = values.map(f);
            let mut percentiles = BTreeMap::new();
            if let Some(p) = percentile {
                percentiles.insert(Percentile::new(99).unwrap(), f(p));
            }
            Statistics::new(StatisticsInput {
                count,
                min,
                max,
                mean,
                median,
                variance,
                std_dev,
                percentiles,
            })
        };
        let cases = [
            (
                0,
                [1.0, 2.0, 1.5, 1.5, 0.0, 0.0],
                None,
                "count must be at least 1",
            ),
            (
                3,
                [2.0, 1.0, 1.5, 1.5, 0.0, 0.0],
                None,
                "min is greater than max",
            ),
            (3, [1.0, 2.0, 2.5, 1.5, 0.0, 0.0], None, "mean is outside"),
            (3, [1.0, 2.0, 1.5, 0.5, 0.0, 0.0], None, "median is outside"),
            (
                3,
                [1.0, 2.0, 1.5, 1.5, -0.1, 0.0],
                None,
                "variance is negative",
            ),
            (
                3,
                [1.0, 2.0, 1.5, 1.5, 0.0, -1.0],
                None,
                "std_dev is negative",
            ),
            (
                3,
                [1.0, 2.0, 1.5, 1.5, 0.0, 0.0],
                Some(9.0),
                "percentile is outside",
            ),
        ];
        for (count, values, percentile, want) in cases {
            let err = make(count, values, percentile).expect_err(want);
            assert_eq!(err.code(), "INCONSISTENT_STATISTICS");
            assert!(err.to_string().contains(want), "got {err}, want {want}");
        }
        assert!(make(3, [1.0, 2.0, 1.5, 1.5, 0.0, 0.0], Some(2.0)).is_ok());
    }

    #[test]
    fn statistics_serde_with_integer_keyed_percentiles() {
        let json = serde_json::to_string(&stats()).unwrap();
        assert!(
            json.contains(r#""percentiles":{"50":70.0,"95":88.0}"#),
            "{json}"
        );
        assert_eq!(serde_json::from_str::<Statistics>(&json).unwrap(), stats());

        let no_percentiles = r#"{"count":2,"min":1.0,"max":3.0,"mean":2.0,"median":2.0,"variance":1.0,"std_dev":1.0}"#;
        assert!(
            serde_json::from_str::<Statistics>(no_percentiles)
                .unwrap()
                .percentiles()
                .is_empty()
        );

        for bad in [
            r#"{"count":0,"min":1.0,"max":3.0,"mean":2.0,"median":2.0,"variance":1.0,"std_dev":1.0}"#,
            r#"{"count":2,"min":1.0,"max":3.0,"mean":2.0,"median":2.0,"variance":1.0,"std_dev":1.0,"percentiles":{"0":1.0}}"#,
            r#"{"count":2,"min":1.0,"max":3.0,"mean":2.0,"median":2.0,"variance":1.0,"std_dev":1.0,"mode":2.0}"#,
            r#"{"count":-1,"min":1.0,"max":3.0,"mean":2.0,"median":2.0,"variance":1.0,"std_dev":1.0}"#,
        ] {
            assert!(serde_json::from_str::<Statistics>(bad).is_err(), "{bad}");
        }
    }

    #[test]
    fn results_accept_anomalies_inside_the_window_only() {
        let ok = result(vec![
            anomaly("2026-09-06T12:00:00Z"),
            anomaly("2026-09-06T12:59:59.999999999Z"),
        ])
        .unwrap();
        assert_eq!(ok.anomalies().len(), 2);
        assert_eq!(ok.window_end(), Some(ts("2026-09-06T13:00:00Z")));
        assert_eq!(ok.service_version(), "abc123");

        let before = result(vec![anomaly("2026-09-06T11:59:59Z")]).unwrap_err();
        assert!(before.to_string().contains("outside the result's window"));
        let at_end = result(vec![anomaly("2026-09-06T13:00:00Z")]).unwrap_err();
        assert_eq!(at_end.code(), "INCONSISTENT_RESULT");

        let mut wrong_type = anomaly("2026-09-06T12:30:00Z");
        wrong_type.measurement_type = MeasurementType::Spo2;
        assert!(
            result(vec![wrong_type])
                .unwrap_err()
                .to_string()
                .contains("measurement type")
        );

        let mut wrong_window = anomaly("2026-09-06T12:30:00Z");
        wrong_window.window = Window::OneMinute;
        assert!(
            result(vec![wrong_window])
                .unwrap_err()
                .to_string()
                .contains("window")
        );

        let mut wrong_version = anomaly("2026-09-06T12:30:00Z");
        wrong_version.algorithm_version = AlgorithmVersion::new(2, 0, 0);
        assert!(
            result(vec![wrong_version])
                .unwrap_err()
                .to_string()
                .contains("algorithm version")
        );
    }

    #[test]
    fn results_require_a_service_version() {
        let mut input = result_input(vec![]);
        input.service_version = "  ".to_owned();
        let err = ProcessingResult::new(input).unwrap_err();
        assert!(err.to_string().contains("service_version"));
    }

    #[test]
    fn result_serde_round_trip_and_validation() {
        let r = result(vec![anomaly("2026-09-06T12:30:00Z")]).unwrap();
        let json = serde_json::to_string(&r).unwrap();
        assert!(json.contains(r#""metric":"Z_SCORE""#) && json.contains(r#""severity":"WARNING""#));
        assert_eq!(serde_json::from_str::<ProcessingResult>(&json).unwrap(), r);

        let outside = json.replace("2026-09-06T12:30:00Z", "2026-09-07T12:30:00Z");
        let err = serde_json::from_str::<ProcessingResult>(&outside).unwrap_err();
        assert!(
            err.to_string().contains("outside the result's window"),
            "{err}"
        );

        let unknown = json.replacen('{', r#"{"patient":"p",""#, 1);
        assert!(serde_json::from_str::<ProcessingResult>(&unknown).is_err());
    }
}
