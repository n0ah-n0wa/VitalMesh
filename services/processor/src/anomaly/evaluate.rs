//! Evaluation of each rule kind over one window's time-ordered values.

use std::collections::VecDeque;

use crate::domain::{Anomaly, AnomalyMetric, Finite, Measurement, Severity, Statistics, Window};
use crate::stats::spread;

use super::rules::{RollingDeviationRule, ThresholdRule, ZScoreRule};
use super::{ALGORITHM_VERSION, AnomalyError, Detection};

fn detection(
    order: usize,
    m: &Measurement,
    window: Window,
    metric: AnomalyMetric,
    threshold: Finite,
    severity: Severity,
) -> Detection {
    Detection {
        order,
        detected_at: m.recorded_at(),
        anomaly: Anomaly {
            measurement_type: m.measurement_type(),
            window,
            metric,
            value: m.value().finite(),
            threshold,
            severity,
            detected_at: m.recorded_at(),
            algorithm_version: ALGORITHM_VERSION,
        },
    }
}

/// Flags every value outside the widest tier it crosses.
pub(super) fn threshold(
    rule: &ThresholdRule,
    items: &[&Measurement],
    window: Window,
    out: &mut Vec<Detection>,
) {
    let order = out.len();
    for m in items {
        let value = m.value().finite();
        if let Some((severity, bound)) = rule
            .tiers
            .descending()
            .find_map(|(severity, bounds)| bounds.crossed_by(value).map(|b| (severity, b)))
        {
            out.push(detection(
                order,
                m,
                window,
                AnomalyMetric::Threshold,
                bound,
                severity,
            ));
        }
    }
}

/// The bound `mean ± t * std` on the side of `value`.
fn bound_from(mean: f64, std_dev: f64, t: f64, value: f64) -> Result<Finite, AnomalyError> {
    let bound = if value >= mean {
        mean + t * std_dev
    } else {
        mean - t * std_dev
    };
    Finite::new(bound).map_err(|_| AnomalyError::NonFinite { what: "bound" })
}

/// Which tier `deviation` exceeds relative to `std_dev`, if any:
/// `deviation > t * std_dev` for the largest `t`.
fn tier_exceeded(
    tiers: &super::rules::Tiers<Finite>,
    deviation: f64,
    std_dev: f64,
) -> Option<(Severity, f64)> {
    tiers
        .descending()
        .find(|(_, t)| deviation > t.get() * std_dev)
        .map(|(severity, t)| (severity, t.get()))
}

/// Flags values whose z-score over the window's own statistics exceeds a
/// tier. A window with fewer than `min_count` values or with zero spread
/// produces nothing.
pub(super) fn z_score(
    rule: &ZScoreRule,
    items: &[&Measurement],
    window: Window,
    stats: Option<&Statistics>,
    out: &mut Vec<Detection>,
) -> Result<(), AnomalyError> {
    let Some(stats) = stats else {
        return Ok(());
    };
    if (stats.count() as usize) < rule.min_count.get() || stats.std_dev() == Finite::ZERO {
        return Ok(());
    }
    let mean = stats.mean().get();
    let std_dev = stats.std_dev().get();
    let order = out.len();
    for m in items {
        let value = m.value().finite().get();
        let deviation = (value - mean).abs();
        if let Some((severity, t)) = tier_exceeded(&rule.tiers, deviation, std_dev) {
            let bound = bound_from(mean, std_dev, t, value)?;
            out.push(detection(
                order,
                m,
                window,
                AnomalyMetric::ZScore,
                bound,
                severity,
            ));
        }
    }
    Ok(())
}

/// Flags values that depart from the mean of the values preceding them by
/// more than a tier times their standard deviation. The baseline holds at
/// most `window_size` predecessors and needs `min_count` of them and a
/// non-zero spread before a value is judged. Each step is `O(window_size)`
/// with no sorting and no allocation: the baseline needs only its mean
/// and standard deviation, computed by the same routine as every other
/// statistic in the engine.
pub(super) fn rolling(
    rule: &RollingDeviationRule,
    items: &[&Measurement],
    window: Window,
    out: &mut Vec<Detection>,
) -> Result<(), AnomalyError> {
    let order = out.len();
    let size = rule.window_size.get();
    let mut baseline: VecDeque<f64> = VecDeque::with_capacity(size);
    for m in items {
        let value = m.value().finite();
        if baseline.len() >= rule.min_count.get()
            && let Some(summary) = spread(baseline.iter().copied())?
            && summary.std_dev != Finite::ZERO
        {
            let mean = summary.mean.get();
            let std_dev = summary.std_dev.get();
            let deviation = (value.get() - mean).abs();
            if let Some((severity, t)) = tier_exceeded(&rule.tiers, deviation, std_dev) {
                let bound = bound_from(mean, std_dev, t, value.get())?;
                out.push(detection(
                    order,
                    m,
                    window,
                    AnomalyMetric::RollingDeviation,
                    bound,
                    severity,
                ));
            }
        }
        if baseline.len() == size {
            baseline.pop_front();
        }
        baseline.push_back(value.get());
    }
    Ok(())
}

#[cfg(test)]
mod tests {
    use std::num::NonZeroUsize;

    use super::*;
    use crate::anomaly::rules::{Bounds, Tiers};
    use crate::domain::{MeasurementId, MeasurementType, MeasurementValue, Percentile, Timestamp};
    use crate::stats::statistics;

    fn fin(v: f64) -> Finite {
        Finite::new(v).unwrap()
    }

    fn series(kind: MeasurementType, values: &[f64]) -> Vec<Measurement> {
        let base = Timestamp::parse("2026-09-06T12:00:00Z").unwrap();
        values
            .iter()
            .enumerate()
            .map(|(i, &v)| {
                Measurement::new(
                    MeasurementId::parse(&format!("m-{i:03}")).unwrap(),
                    kind,
                    MeasurementValue::new(v).unwrap(),
                    kind.canonical_unit(),
                    base.checked_add(std::time::Duration::from_secs(i as u64))
                        .unwrap(),
                )
                .unwrap()
            })
            .collect()
    }

    fn refs(items: &[Measurement]) -> Vec<&Measurement> {
        items.iter().collect()
    }

    fn tiers(info: Option<f64>, warning: Option<f64>, critical: Option<f64>) -> Tiers<Finite> {
        Tiers {
            info: info.map(fin),
            warning: warning.map(fin),
            critical: critical.map(fin),
        }
    }

    #[test]
    fn threshold_picks_the_widest_crossed_tier_and_reports_its_bound() {
        let rule = ThresholdRule {
            tiers: Tiers {
                info: Some(Bounds {
                    lower: Some(fin(60.0)),
                    upper: Some(fin(100.0)),
                }),
                warning: Some(Bounds {
                    lower: Some(fin(50.0)),
                    upper: Some(fin(120.0)),
                }),
                critical: Some(Bounds {
                    lower: Some(fin(40.0)),
                    upper: Some(fin(150.0)),
                }),
            },
        };
        let items = series(
            MeasurementType::HeartRate,
            &[70.0, 59.0, 45.0, 39.0, 100.0, 101.0, 121.0, 151.0],
        );
        let mut out = Vec::new();
        threshold(&rule, &refs(&items), Window::OneHour, &mut out);
        let got: Vec<(f64, f64, Severity)> = out
            .iter()
            .map(|d| {
                (
                    d.anomaly.value.get(),
                    d.anomaly.threshold.get(),
                    d.anomaly.severity,
                )
            })
            .collect();
        assert_eq!(
            got,
            [
                (59.0, 60.0, Severity::Info),
                (45.0, 50.0, Severity::Warning),
                (39.0, 40.0, Severity::Critical),
                (101.0, 100.0, Severity::Info),
                (121.0, 120.0, Severity::Warning),
                (151.0, 150.0, Severity::Critical),
            ]
        );
        for d in &out {
            assert_eq!(d.anomaly.metric, AnomalyMetric::Threshold);
            assert_eq!(d.anomaly.window, Window::OneHour);
            assert_eq!(d.anomaly.algorithm_version, ALGORITHM_VERSION);
            assert_eq!(d.anomaly.measurement_type, MeasurementType::HeartRate);
        }
        assert_eq!(out[0].anomaly.detected_at, items[1].recorded_at());
    }

    #[test]
    fn z_score_uses_window_statistics_and_reports_the_bound_in_value_units() {
        // Eight 10s and one 19: mean 11, sample std exactly 3; z(19) = 8/3.
        let items = series(
            MeasurementType::HeartRate,
            &[10.0, 10.0, 10.0, 10.0, 10.0, 10.0, 10.0, 10.0, 19.0],
        );
        let values: Vec<Finite> = items.iter().map(|m| m.value().finite()).collect();
        let stats = statistics(&values, &[]).unwrap().unwrap();
        assert_eq!(stats.std_dev().get(), 3.0);
        let rule = ZScoreRule {
            min_count: NonZeroUsize::new(5).unwrap(),
            tiers: tiers(Some(2.0), Some(2.5), Some(3.0)),
        };
        let mut out = Vec::new();
        z_score(
            &rule,
            &refs(&items),
            Window::OneHour,
            Some(&stats),
            &mut out,
        )
        .unwrap();
        assert_eq!(out.len(), 1);
        let a = &out[0].anomaly;
        assert_eq!(a.value.get(), 19.0);
        assert_eq!(a.threshold.get(), 18.5, "mean 11 + 2.5 * std 3");
        assert_eq!(a.severity, Severity::Warning);
        assert_eq!(a.metric, AnomalyMetric::ZScore);

        // Below the mean the bound is on the low side.
        let low = series(
            MeasurementType::HeartRate,
            &[10.0, 10.0, 10.0, 10.0, 10.0, 10.0, 10.0, 10.0, 1.0],
        );
        let values: Vec<Finite> = low.iter().map(|m| m.value().finite()).collect();
        let stats = statistics(&values, &[]).unwrap().unwrap();
        let mut out = Vec::new();
        z_score(&rule, &refs(&low), Window::OneHour, Some(&stats), &mut out).unwrap();
        assert_eq!(out.len(), 1);
        assert_eq!(out[0].anomaly.value.get(), 1.0);
        assert_eq!(out[0].anomaly.threshold.get(), 9.0 - 2.5 * 3.0);
    }

    #[test]
    fn z_score_needs_enough_values_and_spread() {
        let rule = ZScoreRule {
            min_count: NonZeroUsize::new(10).unwrap(),
            tiers: tiers(Some(1.0), None, None),
        };
        let items = series(MeasurementType::Spo2, &[90.0, 90.0, 90.0, 99.0]);
        let values: Vec<Finite> = items.iter().map(|m| m.value().finite()).collect();
        let stats = statistics(&values, &[]).unwrap().unwrap();
        let mut out = Vec::new();
        z_score(
            &rule,
            &refs(&items),
            Window::OneHour,
            Some(&stats),
            &mut out,
        )
        .unwrap();
        assert!(out.is_empty(), "fewer than min_count values");

        let constant = series(MeasurementType::Spo2, &[95.0; 20]);
        let values: Vec<Finite> = constant.iter().map(|m| m.value().finite()).collect();
        let stats = statistics(&values, &[]).unwrap().unwrap();
        let mut out = Vec::new();
        z_score(
            &rule,
            &refs(&constant),
            Window::OneHour,
            Some(&stats),
            &mut out,
        )
        .unwrap();
        assert!(out.is_empty(), "zero spread has no z-scores");

        let mut out = Vec::new();
        z_score(&rule, &refs(&constant), Window::OneHour, None, &mut out).unwrap();
        assert!(out.is_empty(), "no statistics, nothing to compare against");
    }

    #[test]
    fn rolling_judges_each_value_against_its_predecessors_only() {
        // Baseline [9, 9, 11, 13, 13]: mean 11, sample std exactly 2.
        // 16 deviates by 5 > 2 * 2 (warning) but not > 3 * 2 (critical).
        let items = series(
            MeasurementType::HeartRate,
            &[9.0, 9.0, 11.0, 13.0, 13.0, 16.0, 11.0],
        );
        let rule = RollingDeviationRule {
            window_size: NonZeroUsize::new(5).unwrap(),
            min_count: NonZeroUsize::new(5).unwrap(),
            tiers: tiers(None, Some(2.0), Some(3.0)),
        };
        let mut out = Vec::new();
        rolling(&rule, &refs(&items), Window::SixHours, &mut out).unwrap();
        assert_eq!(out.len(), 1, "{out:?}");
        let a = &out[0].anomaly;
        assert_eq!(a.value.get(), 16.0);
        assert_eq!(a.threshold.get(), 15.0, "mean 11 + 2 * std 2");
        assert_eq!(a.severity, Severity::Warning);
        assert_eq!(a.metric, AnomalyMetric::RollingDeviation);
        assert_eq!(a.detected_at, items[5].recorded_at());
    }

    #[test]
    fn rolling_warm_up_and_constant_baselines_produce_nothing() {
        let rule = RollingDeviationRule {
            window_size: NonZeroUsize::new(3).unwrap(),
            min_count: NonZeroUsize::new(3).unwrap(),
            tiers: tiers(Some(0.5), None, None),
        };
        let short = series(MeasurementType::HeartRate, &[10.0, 100.0]);
        let mut out = Vec::new();
        rolling(&rule, &refs(&short), Window::OneHour, &mut out).unwrap();
        assert!(out.is_empty(), "no baseline yet");

        let flat = series(MeasurementType::HeartRate, &[10.0, 10.0, 10.0, 100.0]);
        let mut out = Vec::new();
        rolling(&rule, &refs(&flat), Window::OneHour, &mut out).unwrap();
        assert!(out.is_empty(), "constant baseline has zero spread");

        let empty: Vec<Measurement> = Vec::new();
        let mut out = Vec::new();
        rolling(&rule, &refs(&empty), Window::OneHour, &mut out).unwrap();
        assert!(out.is_empty());
    }

    #[test]
    fn rolling_baseline_is_bounded_to_window_size() {
        // With window_size 2 the baseline forgets early values: after
        // [50, 50] every later 50 is judged against [50, 50] (no spread).
        let rule = RollingDeviationRule {
            window_size: NonZeroUsize::new(2).unwrap(),
            min_count: NonZeroUsize::new(2).unwrap(),
            tiers: tiers(Some(1.0), None, None),
        };
        let items = series(MeasurementType::HeartRate, &[10.0, 90.0, 50.0, 50.0, 50.0]);
        let mut out = Vec::new();
        rolling(&rule, &refs(&items), Window::OneHour, &mut out).unwrap();
        // 50 vs [10, 90]: mean 50, deviation 0 -> nothing.
        // 50 vs [90, 50]: mean 70, std ~28.28, deviation 20 < 28.28 -> nothing.
        // 50 vs [50, 50]: zero spread -> nothing.
        assert!(out.is_empty(), "{out:?}");
    }

    #[test]
    fn bound_overflow_is_an_error_not_a_result() {
        let err = bound_from(f64::MAX, f64::MAX, 2.0, f64::MAX).unwrap_err();
        assert!(
            matches!(err, AnomalyError::NonFinite { what: "bound" }),
            "{err}"
        );
        assert_eq!(bound_from(10.0, 2.0, 3.0, 20.0).unwrap().get(), 16.0);
        assert_eq!(bound_from(10.0, 2.0, 3.0, 0.0).unwrap().get(), 4.0);
    }

    #[test]
    fn percentiles_do_not_influence_detection() {
        let items = series(
            MeasurementType::HeartRate,
            &[10.0, 10.0, 10.0, 10.0, 10.0, 10.0, 10.0, 10.0, 19.0],
        );
        let values: Vec<Finite> = items.iter().map(|m| m.value().finite()).collect();
        let with = statistics(&values, &[Percentile::new(90).unwrap()])
            .unwrap()
            .unwrap();
        let without = statistics(&values, &[]).unwrap().unwrap();
        let rule = ZScoreRule {
            min_count: NonZeroUsize::new(2).unwrap(),
            tiers: tiers(Some(2.0), None, None),
        };
        let mut a = Vec::new();
        let mut b = Vec::new();
        z_score(&rule, &refs(&items), Window::OneHour, Some(&with), &mut a).unwrap();
        z_score(
            &rule,
            &refs(&items),
            Window::OneHour,
            Some(&without),
            &mut b,
        )
        .unwrap();
        assert_eq!(a.len(), 1);
        assert_eq!(a[0].anomaly, b[0].anomaly);
    }
}
