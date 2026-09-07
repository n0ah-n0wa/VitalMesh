//! The statistical processing engine (SPECIFICATIONS.md sections 14 and 15).
//!
//! The engine turns time-ordered measurements into descriptive statistics
//! per measurement type and tumbling time window. It is deterministic:
//! identical input in identical order with identical parameters yields
//! bit-identical output on any platform, because every operation is a
//! fixed sequence of IEEE-754 binary64 operations with no data-dependent
//! parallelism and no hashing of floats.
//!
//! # Numerical behaviour
//!
//! Every rule is explicit so that results can be reproduced elsewhere.
//!
//! - **Inputs** are [`Finite`] values: never NaN or infinite, with `-0.0`
//!   normalised to `0.0`. Comparisons use the total order of `f64`.
//! - **Sums** use Neumaier's compensated summation in input order, which
//!   bounds the rounding error independently of the sample size.
//! - **Mean** is the compensated sum divided by the count, then clamped to
//!   `[min, max]`: for a sample of identical values the division can round
//!   one ULP past the value, and the clamp restores the invariant that a
//!   mean lies within the observed range.
//! - **Variance** is the *sample* variance (Bessel's correction, `n - 1`),
//!   computed in two passes: deviations from the mean, squared, summed
//!   with compensation. A sample of one value has variance `0`. A rounding
//!   result below zero is clamped to `0`.
//! - **Standard deviation** is the square root of the variance.
//! - **Median** is the middle value of the sorted sample for an odd count,
//!   and the midpoint `lo + (hi - lo) / 2` of the two middle values for an
//!   even count, which cannot leave `[lo, hi]`.
//! - **Percentiles** use the nearest-rank method on the sorted sample: the
//!   value at rank `ceil(p / 100 * n)`, clamped to `[1, n]`. Every
//!   percentile is therefore an observed value.
//! - **Windows** are tumbling and aligned to the Unix epoch in UTC: a
//!   timestamp `t` falls in the window starting at
//!   `floor(t / width) * width`. Windows without data produce nothing.
//! - **Rolling statistics** are computed over the last `k` samples; the
//!   first `k - 1` points use the samples available so far.
//!
//! # Memory
//!
//! Aggregation holds the measurements it was given plus one sorted copy of
//! the values of the window being processed; rolling statistics hold `k`
//! values. Nothing grows with the number of windows or with time. The
//! per-job ceiling on measurements is enforced by the caller.
//!
//! Module map:
//! - `order`: time ordering with deterministic tie-breaking.
//! - `summary`: descriptive statistics of one sample.
//! - `rolling`: rolling mean and moving standard deviation.
//! - `window`: epoch-aligned tumbling windows and the aggregation pipeline.

mod order;
mod rolling;
mod summary;
mod window;

use std::fmt;

use crate::domain::{DomainError, Finite};

pub use order::time_order;
pub use rolling::{Rolling, RollingPoint};
pub use summary::{Spread, Summary, describe, percentiles, spread, statistics};
pub use window::{WindowSlice, WindowStatistics, aggregate, tumbling, window_start};

/// Why a statistic could not be produced. Every case is a defect in the
/// input rather than a runtime condition: samples are finite and bounded
/// by construction, so callers treat these as internal errors.
#[derive(Debug, Clone, PartialEq, Eq)]
pub enum StatsError {
    /// An intermediate value overflowed to infinity.
    NonFinite { what: &'static str },
    /// The computed values violate a relation the domain enforces.
    Inconsistent(DomainError),
}

impl fmt::Display for StatsError {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        match self {
            Self::NonFinite { what } => write!(f, "{what} is not finite"),
            Self::Inconsistent(err) => write!(f, "inconsistent statistics: {err}"),
        }
    }
}

impl std::error::Error for StatsError {}

impl From<DomainError> for StatsError {
    fn from(err: DomainError) -> Self {
        Self::Inconsistent(err)
    }
}

/// Neumaier's compensated summation: a running sum plus a running
/// compensation term, so that the error is bounded independently of how
/// many values are added. Values must be added in the order the sum is
/// defined over; every sum in the engine goes through this accumulator.
#[derive(Debug, Clone, Copy, Default)]
pub(crate) struct Neumaier {
    sum: f64,
    compensation: f64,
}

impl Neumaier {
    #[inline]
    pub(crate) fn add(&mut self, value: f64) {
        let t = self.sum + value;
        if self.sum.abs() >= value.abs() {
            self.compensation += (self.sum - t) + value;
        } else {
            self.compensation += (value - t) + self.sum;
        }
        self.sum = t;
    }

    /// The compensated total; finite unless the true sum is out of range.
    #[inline]
    pub(crate) fn total(self) -> f64 {
        self.sum + self.compensation
    }
}

pub(crate) fn finite(value: f64, what: &'static str) -> Result<Finite, StatsError> {
    Finite::new(value).map_err(|_| StatsError::NonFinite { what })
}
