//! Rolling mean and moving standard deviation over the last `k` samples.
//!
//! The window holds at most `k` values in a ring buffer and recomputes the
//! statistics from those values at every step, without sorting or
//! allocating. That is `O(k)` per point rather than `O(1)`, but it is
//! exact, deterministic and free of the cancellation that incremental
//! removal introduces; the benchmark suite measures it so that a faster
//! scheme can be justified later.

use std::collections::VecDeque;
use std::num::NonZeroUsize;

use crate::domain::Finite;

use super::{StatsError, spread};

/// The rolling statistics at one position of the input.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub struct RollingPoint {
    /// Position in the input, starting at 0.
    pub index: usize,
    /// Number of samples the point is computed over: `min(index + 1, k)`.
    pub count: usize,
    pub mean: Finite,
    /// Sample standard deviation of the window; `0` while `count < 2`.
    pub std_dev: Finite,
}

/// An iterator producing one [`RollingPoint`] per input value.
pub struct Rolling<I> {
    values: I,
    size: usize,
    window: VecDeque<f64>,
    index: usize,
}

impl<I> Rolling<I>
where
    I: Iterator<Item = Finite>,
{
    /// Rolls a window of `size` samples over `values`. The first `size - 1`
    /// points cover the samples available so far.
    pub fn new(values: impl IntoIterator<IntoIter = I>, size: NonZeroUsize) -> Self {
        Self {
            values: values.into_iter(),
            size: size.get(),
            window: VecDeque::with_capacity(size.get()),
            index: 0,
        }
    }
}

impl<I> Iterator for Rolling<I>
where
    I: Iterator<Item = Finite>,
{
    type Item = Result<RollingPoint, StatsError>;

    fn next(&mut self) -> Option<Self::Item> {
        let value = self.values.next()?;
        if self.window.len() == self.size {
            self.window.pop_front();
        }
        self.window.push_back(value.get());
        let index = self.index;
        self.index += 1;
        match point(index, &self.window) {
            Ok(Some(point)) => Some(Ok(point)),
            Ok(None) => None,
            Err(err) => Some(Err(err)),
        }
    }

    fn size_hint(&self) -> (usize, Option<usize>) {
        self.values.size_hint()
    }
}

/// The point over `window`, or `None` for an empty window (which `next`
/// never produces, since it pushes before it computes).
fn point(index: usize, window: &VecDeque<f64>) -> Result<Option<RollingPoint>, StatsError> {
    // `spread` is the same computation `describe` uses, so a rolling point
    // and a full description of the window's values agree bit for bit.
    Ok(spread(window.iter().copied())?.map(|spread| RollingPoint {
        index,
        count: spread.count as usize,
        mean: spread.mean,
        std_dev: spread.std_dev,
    }))
}

#[cfg(test)]
mod tests {
    use super::*;

    fn fin(values: &[f64]) -> Vec<Finite> {
        values.iter().map(|&v| Finite::new(v).unwrap()).collect()
    }

    fn roll(values: &[f64], k: usize) -> Vec<RollingPoint> {
        Rolling::new(fin(values), NonZeroUsize::new(k).unwrap())
            .collect::<Result<_, _>>()
            .unwrap()
    }

    #[test]
    fn empty_input_yields_no_points() {
        assert!(roll(&[], 3).is_empty());
    }

    #[test]
    fn single_value_and_window_of_one() {
        let points = roll(&[42.0], 5);
        assert_eq!(points.len(), 1);
        assert_eq!(points[0].count, 1);
        assert_eq!(points[0].mean.get(), 42.0);
        assert_eq!(points[0].std_dev, Finite::ZERO);

        let points = roll(&[1.0, 2.0, 3.0], 1);
        let means: Vec<f64> = points.iter().map(|p| p.mean.get()).collect();
        assert_eq!(means, [1.0, 2.0, 3.0]);
        assert!(
            points
                .iter()
                .all(|p| p.std_dev == Finite::ZERO && p.count == 1)
        );
    }

    #[test]
    fn rolling_mean_and_moving_std_over_three() {
        let points = roll(&[1.0, 2.0, 3.0, 4.0, 5.0], 3);
        let means: Vec<f64> = points.iter().map(|p| p.mean.get()).collect();
        assert_eq!(means, [1.0, 1.5, 2.0, 3.0, 4.0]);
        let counts: Vec<usize> = points.iter().map(|p| p.count).collect();
        assert_eq!(counts, [1, 2, 3, 3, 3]);
        let std: Vec<f64> = points.iter().map(|p| p.std_dev.get()).collect();
        // Sample std of {1,2}: sqrt(0.5); of {1,2,3}, {2,3,4}, {3,4,5}: 1.
        assert_eq!(std[0], 0.0);
        assert_eq!(std[1], 0.5_f64.sqrt());
        assert_eq!(&std[2..], &[1.0, 1.0, 1.0]);
        let indexes: Vec<usize> = points.iter().map(|p| p.index).collect();
        assert_eq!(indexes, [0, 1, 2, 3, 4]);
    }

    #[test]
    fn window_larger_than_input_behaves_like_cumulative() {
        let points = roll(&[2.0, 4.0, 6.0], 10);
        let means: Vec<f64> = points.iter().map(|p| p.mean.get()).collect();
        assert_eq!(means, [2.0, 3.0, 4.0]);
        assert_eq!(points[2].count, 3);
    }

    #[test]
    fn constant_input_has_zero_deviation_everywhere() {
        let points = roll(&[0.1; 50], 7);
        assert!(points.iter().all(|p| p.mean.get() == 0.1));
        assert!(points.iter().all(|p| p.std_dev == Finite::ZERO));
    }

    #[test]
    fn is_lazy_and_bounded() {
        let size = NonZeroUsize::new(4).unwrap();
        let values = (0..1_000_000).map(|i| Finite::new(i as f64).unwrap());
        let mut rolling = Rolling::new(values, size);
        let last = rolling.by_ref().take(10).last().unwrap().unwrap();
        assert_eq!(last.index, 9);
        assert_eq!(last.count, 4);
        assert_eq!(last.mean.get(), 7.5);
        assert!(rolling.window.len() <= 4);
    }

    #[test]
    fn determinism() {
        let a = roll(&[3.0, 1.0, 4.0, 1.0, 5.0, 9.0, 2.0, 6.0], 4);
        let b = roll(&[3.0, 1.0, 4.0, 1.0, 5.0, 9.0, 2.0, 6.0], 4);
        assert_eq!(a, b);
    }

    #[test]
    fn overflow_is_reported() {
        let err = Rolling::new(fin(&[f64::MAX, f64::MAX]), NonZeroUsize::new(2).unwrap())
            .last()
            .unwrap()
            .unwrap_err();
        assert!(matches!(err, StatsError::NonFinite { .. }));
    }
}
