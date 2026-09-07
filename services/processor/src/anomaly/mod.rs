//! Configurable anomaly detection (SPECIFICATIONS.md section 16).
//!
//! An anomaly is a value that departs from expectation by a configured
//! margin. This is technical anomaly detection over synthetic data: the
//! severities INFO, WARNING and CRITICAL classify how far a value departs
//! from a rule's expectation and carry no medical meaning.
//!
//! # Rules
//!
//! A [`RuleSet`] holds rules of three kinds, each scoped to measurement
//! types and windows (empty scope means every type or window) and each
//! carrying severity tiers. The highest tier a value exceeds determines the
//! severity; a value that exceeds no tier is not anomalous.
//!
//! - **Threshold**: `value < lower` or `value > upper` for the tier's
//!   bounds; outer tiers must contain inner ones.
//! - **Z-score**: `|value - mean| / std_dev > t` over the window's own
//!   statistics. Requires at least `min_count` values and a non-zero
//!   standard deviation: a constant window has no z-scores and produces
//!   nothing.
//! - **Rolling deviation**: `|value - rolling_mean| > t * rolling_std`
//!   where the rolling statistics come from the `window_size` values that
//!   precede the value, so the value under test never influences its own
//!   baseline. The first values of a window have no baseline and produce
//!   nothing until `min_count` predecessors exist.
//!
//! # Results
//!
//! Every [`Anomaly`] reports the measured value and the *bound* it crossed
//! in the measurement's own unit: the tier's bound for threshold rules,
//! `mean ± t * std_dev` for z-score rules and `rolling_mean ± t *
//! rolling_std` for rolling rules. `detected_at` is the measurement's
//! `recorded_at`, never the wall clock, and `algorithm_version` is
//! [`ALGORITHM_VERSION`], so identical input, rules and version yield
//! bit-identical output.
//!
//! # Numerical behaviour
//!
//! Inputs are finite. Bounds are computed as `mean + t * std` in binary64
//! and are checked for finiteness; an overflow is reported as an error,
//! never emitted. Comparisons are strict, as the specification writes them,
//! so a value exactly on a bound is not anomalous.

mod evaluate;
mod rules;

use std::fmt;

use crate::domain::{
    AlgorithmVersion, Anomaly, DomainError, Measurement, MeasurementType, ProcessingParameters,
    Statistics, Timestamp, Window,
};
use crate::stats::{StatsError, statistics, time_order, tumbling};

pub use rules::{
    Bounds, RollingDeviationRule, Rule, RuleKind, RuleSet, ThresholdRule, Tiers, ZScoreRule,
};

/// The version of the detection algorithms in this module. Bump the major
/// number for any change that can alter which anomalies are produced or
/// their values; results carry it so they are never compared across
/// versions.
pub const ALGORITHM_VERSION: AlgorithmVersion = AlgorithmVersion::new(1, 0, 0);

/// Why detection could not run.
#[derive(Debug)]
pub enum AnomalyError {
    /// A computed bound left the finite range.
    NonFinite { what: &'static str },
    /// The window statistics could not be computed.
    Stats(StatsError),
    /// The caller's checkpoint stopped the run (cancellation, deadline).
    Interrupted(crate::error::Error),
}

impl fmt::Display for AnomalyError {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        match self {
            Self::NonFinite { what } => write!(f, "{what} is not finite"),
            Self::Stats(err) => write!(f, "statistics: {err}"),
            Self::Interrupted(err) => write!(f, "interrupted: {err}"),
        }
    }
}

impl std::error::Error for AnomalyError {}

impl From<StatsError> for AnomalyError {
    fn from(err: StatsError) -> Self {
        Self::Stats(err)
    }
}

impl From<DomainError> for AnomalyError {
    fn from(err: DomainError) -> Self {
        Self::Stats(StatsError::Inconsistent(err))
    }
}

/// Evaluates a validated rule set.
#[derive(Debug, Clone)]
pub struct Detector {
    rules: RuleSet,
}

impl Detector {
    pub fn new(rules: RuleSet) -> Self {
        Self { rules }
    }

    pub fn rules(&self) -> &RuleSet {
        &self.rules
    }

    pub fn algorithm_version(&self) -> AlgorithmVersion {
        ALGORITHM_VERSION
    }

    /// Detects anomalies among `items`, the time-ordered measurements of
    /// one type in one window, using `stats` for the z-score rules. Output
    /// follows input order, then rule order within one measurement.
    pub fn detect(
        &self,
        measurement_type: MeasurementType,
        window: Window,
        items: &[&Measurement],
        stats: Option<&Statistics>,
    ) -> Result<Vec<Anomaly>, AnomalyError> {
        let mut out = Vec::new();
        for rule in self.rules.rules() {
            if !rule.applies_to(measurement_type, window) {
                continue;
            }
            match rule.kind() {
                RuleKind::Threshold(r) => evaluate::threshold(r, items, window, &mut out),
                RuleKind::ZScore(r) => evaluate::z_score(r, items, window, stats, &mut out)?,
                RuleKind::RollingDeviation(r) => evaluate::rolling(r, items, window, &mut out)?,
            }
        }
        // Order by time, then by the order rules were declared, so equal
        // inputs give equal output whatever the evaluation order above.
        out.sort_by(|a, b| {
            a.detected_at
                .cmp(&b.detected_at)
                .then(a.order.cmp(&b.order))
        });
        Ok(out.into_iter().map(|d| d.anomaly).collect())
    }
}

/// One window's statistics and anomalies: the "Result Generation" step.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct WindowReport {
    pub measurement_type: MeasurementType,
    pub window: Window,
    pub window_start: Timestamp,
    pub statistics: Statistics,
    pub anomalies: Vec<Anomaly>,
}

/// Runs the whole pipeline: time ordering, window aggregation, statistics
/// and anomaly detection for every requested type and window. Windows
/// without data produce nothing.
pub fn run(
    measurements: &mut [Measurement],
    parameters: &ProcessingParameters,
    detector: &Detector,
) -> Result<Vec<WindowReport>, AnomalyError> {
    run_with(measurements, parameters, detector, &mut || Ok(()))
}

/// [`run`] with a checkpoint called after every window, so a long run can
/// be stopped cooperatively: an error from `checkpoint` ends the run as
/// [`AnomalyError::Interrupted`].
pub fn run_with(
    measurements: &mut [Measurement],
    parameters: &ProcessingParameters,
    detector: &Detector,
    checkpoint: &mut dyn FnMut() -> crate::error::Result<()>,
) -> Result<Vec<WindowReport>, AnomalyError> {
    time_order(measurements);
    let mut out = Vec::new();
    let mut values = Vec::new();
    for &measurement_type in parameters.measurement_types() {
        let of_type: Vec<&Measurement> = measurements
            .iter()
            .filter(|m| m.measurement_type() == measurement_type)
            .collect();
        for &window in parameters.windows() {
            for slice in tumbling(&of_type, window, |m| m.recorded_at()) {
                values.clear();
                values.extend(slice.items.iter().map(|m| m.value().finite()));
                let Some(stats) = statistics(&values, parameters.percentiles())? else {
                    continue;
                };
                let anomalies =
                    detector.detect(measurement_type, window, slice.items, Some(&stats))?;
                out.push(WindowReport {
                    measurement_type,
                    window,
                    window_start: slice.start,
                    statistics: stats,
                    anomalies,
                });
                checkpoint().map_err(AnomalyError::Interrupted)?;
            }
        }
    }
    Ok(out)
}

/// An anomaly with the declaration order of the rule that produced it, for
/// deterministic sorting.
#[derive(Debug)]
pub(crate) struct Detection {
    pub(crate) order: usize,
    pub(crate) detected_at: Timestamp,
    pub(crate) anomaly: Anomaly,
}
