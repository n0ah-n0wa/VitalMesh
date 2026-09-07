//! Descriptive statistics of one sample.

use std::collections::BTreeMap;

use crate::domain::{Finite, Percentile, Statistics, StatisticsInput};

use super::{Neumaier, StatsError, finite};

/// The descriptive statistics of a non-empty sample, before percentiles.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub struct Summary {
    pub count: u64,
    pub min: Finite,
    pub max: Finite,
    pub mean: Finite,
    pub median: Finite,
    /// Sample variance (`n - 1`); `0` for a single value.
    pub variance: Finite,
    pub std_dev: Finite,
}

/// The statistics of a non-empty sample that need no order statistics:
/// everything in [`Summary`] except the median.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub struct Spread {
    pub count: u64,
    pub min: Finite,
    pub max: Finite,
    pub mean: Finite,
    /// Sample variance (`n - 1`); `0` for a single value.
    pub variance: Finite,
    pub std_dev: Finite,
}

/// Computes the [`Spread`] of `values` in iteration order, or `None` for
/// an empty sample. This is the one place the mean and the variance are
/// computed: [`describe`] and the rolling statistics both call it, so a
/// rolling baseline and a full description of the same values agree bit
/// for bit. It sorts nothing and allocates nothing: `O(n)` in two passes,
/// one for the count, extremes and sum, one for the squared deviations.
#[inline]
pub fn spread(values: impl Iterator<Item = f64> + Clone) -> Result<Option<Spread>, StatsError> {
    let mut n = 0usize;
    let mut min = f64::INFINITY;
    let mut max = f64::NEG_INFINITY;
    let mut sum = Neumaier::default();
    for v in values.clone() {
        n += 1;
        min = min.min(v);
        max = max.max(v);
        sum.add(v);
    }
    if n == 0 {
        return Ok(None);
    }
    let raw_mean = sum.total() / n as f64;
    let mean = finite(raw_mean.clamp(min, max), "mean")?;

    let variance = if n < 2 {
        0.0
    } else {
        let mean = mean.get();
        let mut squares = Neumaier::default();
        for v in values {
            let d = v - mean;
            squares.add(d * d);
        }
        (squares.total() / (n - 1) as f64).max(0.0)
    };
    let variance = finite(variance, "variance")?;
    let std_dev = finite(variance.get().sqrt(), "std_dev")?;
    Ok(Some(Spread {
        count: n as u64,
        min: finite(min, "min")?,
        max: finite(max, "max")?,
        mean,
        variance,
        std_dev,
    }))
}

/// Computes the descriptive statistics of `values`, or `None` for an empty
/// sample. `values` may be in any order; the result does not depend on it
/// except through rounding, which is why callers pass time-ordered input.
pub fn describe(values: &[Finite]) -> Result<Option<Summary>, StatsError> {
    if values.is_empty() {
        return Ok(None);
    }
    let mut sorted = values.to_vec();
    sorted.sort();
    describe_sorted(values, &sorted)
}

/// `describe` for a sample whose sorted copy the caller already holds.
/// `values` supplies the summation order, `sorted` the order statistics.
fn describe_sorted(values: &[Finite], sorted: &[Finite]) -> Result<Option<Summary>, StatsError> {
    let n = values.len();
    let Some(spread) = spread(values.iter().map(|v| v.get()))? else {
        return Ok(None);
    };

    let median = if n % 2 == 1 {
        sorted[n / 2]
    } else {
        let lo = sorted[n / 2 - 1].get();
        let hi = sorted[n / 2].get();
        finite(lo + (hi - lo) / 2.0, "median")?
    };

    Ok(Some(Summary {
        count: spread.count,
        min: spread.min,
        max: spread.max,
        mean: spread.mean,
        median,
        variance: spread.variance,
        std_dev: spread.std_dev,
    }))
}

/// Nearest-rank percentiles of a sorted, non-empty sample: rank
/// `ceil(p / 100 * n)` clamped to `[1, n]`. An empty sample yields an empty
/// map.
pub fn percentiles(sorted: &[Finite], ranks: &[Percentile]) -> BTreeMap<Percentile, Finite> {
    let n = sorted.len();
    if n == 0 {
        return BTreeMap::new();
    }
    ranks
        .iter()
        .map(|&p| {
            // Integer arithmetic keeps the rank exact: ceil(p * n / 100).
            let rank = (usize::from(p.rank()) * n).div_ceil(100).clamp(1, n);
            (p, sorted[rank - 1])
        })
        .collect()
}

/// Computes the validated [`Statistics`] of `values` with the requested
/// percentiles, sorting once. Returns `Ok(None)` for an empty sample.
pub fn statistics(
    values: &[Finite],
    ranks: &[Percentile],
) -> Result<Option<Statistics>, StatsError> {
    if values.is_empty() {
        return Ok(None);
    }
    let mut sorted = values.to_vec();
    sorted.sort();
    let Some(summary) = describe_sorted(values, &sorted)? else {
        return Ok(None);
    };
    let stats = Statistics::new(StatisticsInput {
        count: summary.count,
        min: summary.min,
        max: summary.max,
        mean: summary.mean,
        median: summary.median,
        variance: summary.variance,
        std_dev: summary.std_dev,
        percentiles: percentiles(&sorted, ranks),
    })?;
    Ok(Some(stats))
}

#[cfg(test)]
mod tests {
    use super::*;

    fn fin(values: &[f64]) -> Vec<Finite> {
        values.iter().map(|&v| Finite::new(v).unwrap()).collect()
    }

    fn p(rank: u8) -> Percentile {
        Percentile::new(rank).unwrap()
    }

    #[test]
    fn empty_sample_yields_nothing() {
        assert_eq!(describe(&[]).unwrap(), None);
        assert!(statistics(&[], &[p(50)]).unwrap().is_none());
        assert!(percentiles(&[], &[p(50)]).is_empty());
    }

    #[test]
    fn single_value_is_its_own_everything() {
        let s = describe(&fin(&[36.6])).unwrap().unwrap();
        assert_eq!(s.count, 1);
        for v in [s.min, s.max, s.mean, s.median] {
            assert_eq!(v.get(), 36.6);
        }
        assert_eq!(s.variance, Finite::ZERO);
        assert_eq!(s.std_dev, Finite::ZERO);
        let stats = statistics(&fin(&[36.6]), &[p(1), p(50), p(99)])
            .unwrap()
            .unwrap();
        assert!(stats.percentiles().values().all(|v| v.get() == 36.6));
    }

    #[test]
    fn known_sample() {
        // 2, 4, 4, 4, 5, 5, 7, 9: mean 5, population variance 4, sample variance 32/7.
        let values = fin(&[2.0, 4.0, 4.0, 4.0, 5.0, 5.0, 7.0, 9.0]);
        let s = describe(&values).unwrap().unwrap();
        assert_eq!(s.count, 8);
        assert_eq!(s.min.get(), 2.0);
        assert_eq!(s.max.get(), 9.0);
        assert_eq!(s.mean.get(), 5.0);
        assert_eq!(s.median.get(), 4.5);
        assert_eq!(s.variance.get(), 32.0 / 7.0);
        assert_eq!(s.std_dev.get(), (32.0_f64 / 7.0).sqrt());
    }

    #[test]
    fn median_odd_and_even() {
        assert_eq!(
            describe(&fin(&[3.0, 1.0, 2.0]))
                .unwrap()
                .unwrap()
                .median
                .get(),
            2.0
        );
        assert_eq!(
            describe(&fin(&[4.0, 1.0, 3.0, 2.0]))
                .unwrap()
                .unwrap()
                .median
                .get(),
            2.5
        );
        // The midpoint never leaves [lo, hi], even for values whose plain
        // sum would overflow (half of f64::MAX twice: the compensated sum of
        // the pair still fits, the naive midpoint (lo + hi) / 2 would not).
        let half = f64::MAX / 2.0;
        let extreme = fin(&[half, half]);
        assert_eq!(describe(&extreme).unwrap().unwrap().median.get(), half);
        let wide = fin(&[-half, half]);
        assert_eq!(describe(&wide).unwrap().unwrap().median, Finite::ZERO);
    }

    #[test]
    fn identical_values_have_zero_spread_and_exact_mean() {
        // 0.1 summed three times is 0.30000000000000004; divided by three it
        // rounds above 0.1. The clamp keeps mean == min == max.
        let values = fin(&[0.1, 0.1, 0.1]);
        let s = describe(&values).unwrap().unwrap();
        assert_eq!(s.mean.get(), 0.1);
        assert_eq!(s.variance, Finite::ZERO);
        assert_eq!(s.std_dev, Finite::ZERO);
        let many = fin(&vec![98.6; 10_001]);
        let s = describe(&many).unwrap().unwrap();
        assert_eq!(s.mean.get(), 98.6);
        assert_eq!(s.variance, Finite::ZERO);
    }

    #[test]
    fn compensated_summation_beats_naive_summation() {
        // Many small values after a large one: naive summation loses them.
        let mut values = vec![1e8];
        values.extend(std::iter::repeat_n(1e-8, 1_000_000));
        let naive: f64 = values.iter().sum();
        let compensated = values
            .iter()
            .fold(Neumaier::default(), |mut acc, &v| {
                acc.add(v);
                acc
            })
            .total();
        let exact = 1e8 + 1e-2;
        assert!((compensated - exact).abs() < (naive - exact).abs());
        assert!((compensated - exact).abs() < 1e-9);
    }

    #[test]
    fn nearest_rank_percentiles() {
        let sorted = fin(&[15.0, 20.0, 35.0, 40.0, 50.0]);
        let got = percentiles(&sorted, &[p(5), p(30), p(40), p(50), p(99)]);
        assert_eq!(got[&p(5)].get(), 15.0);
        assert_eq!(got[&p(30)].get(), 20.0);
        assert_eq!(got[&p(40)].get(), 20.0);
        assert_eq!(got[&p(50)].get(), 35.0);
        assert_eq!(got[&p(99)].get(), 50.0);
        // Every percentile is an observed value between min and max.
        for v in got.values() {
            assert!(sorted.contains(v));
        }
        // Rank arithmetic is exact for large samples: p99 of 100 is the 99th.
        let big: Vec<Finite> = (1..=100).map(|i| Finite::new(i as f64).unwrap()).collect();
        assert_eq!(percentiles(&big, &[p(99)])[&p(99)].get(), 99.0);
        assert_eq!(percentiles(&big, &[p(1)])[&p(1)].get(), 1.0);
        assert_eq!(percentiles(&big, &[p(50)])[&p(50)].get(), 50.0);
    }

    #[test]
    fn statistics_are_deterministic_and_order_independent_for_exact_data() {
        let a = fin(&[5.0, 1.0, 4.0, 2.0, 3.0]);
        let b = fin(&[1.0, 2.0, 3.0, 4.0, 5.0]);
        let sa = statistics(&a, &[p(50), p(90)]).unwrap().unwrap();
        let sb = statistics(&b, &[p(50), p(90)]).unwrap().unwrap();
        assert_eq!(sa, sb);
        assert_eq!(sa.mean().get(), 3.0);
        assert_eq!(sa.variance().get(), 2.5);
        let again = statistics(&a, &[p(50), p(90)]).unwrap().unwrap();
        assert_eq!(sa, again);
    }

    #[test]
    fn negative_and_zero_values() {
        let s = describe(&fin(&[-3.0, 0.0, 3.0])).unwrap().unwrap();
        assert_eq!(s.mean, Finite::ZERO);
        assert_eq!(s.median, Finite::ZERO);
        assert_eq!(s.variance.get(), 9.0);
        assert!(s.mean.get().is_sign_positive(), "no negative zero");
    }

    #[test]
    fn overflow_is_reported_not_propagated() {
        let huge = fin(&[f64::MAX, f64::MAX, -f64::MAX]);
        let err = describe(&huge).unwrap_err();
        assert!(matches!(err, StatsError::NonFinite { .. }), "{err}");
    }

    /// `spread` is the sole computation of mean and variance; this guards
    /// the agreement between it and `describe` on data with rounding.
    #[test]
    fn spread_agrees_with_describe_bit_for_bit() {
        let mut state: u64 = 0x2545_F491_4F6C_DD1D;
        let sample: Vec<Finite> = (0..2_001)
            .map(|_| {
                state ^= state << 13;
                state ^= state >> 7;
                state ^= state << 17;
                Finite::new(36.0 + (state % 100_003) as f64 / 7_919.0).unwrap()
            })
            .collect();
        for n in [1usize, 2, 3, 30, 31, 2_000, 2_001] {
            let values = &sample[..n];
            let d = describe(values).unwrap().unwrap();
            let s = spread(values.iter().map(|v| v.get())).unwrap().unwrap();
            assert_eq!(
                (s.count, s.min, s.max, s.mean, s.variance, s.std_dev),
                (d.count, d.min, d.max, d.mean, d.variance, d.std_dev),
                "n = {n}"
            );
        }
        assert_eq!(spread(std::iter::empty()).unwrap(), None);
    }

    #[test]
    fn large_sample_stays_within_bounds() {
        let values: Vec<Finite> = (0..100_000)
            .map(|i| Finite::new(60.0 + (i % 40) as f64 * 0.5).unwrap())
            .collect();
        let s = statistics(&values, &[p(25), p(75)]).unwrap().unwrap();
        assert_eq!(s.count(), 100_000);
        assert!(s.min() <= s.mean() && s.mean() <= s.max());
        assert!(s.min() <= s.median() && s.median() <= s.max());
        assert!(s.std_dev().get() > 0.0);
    }
}
