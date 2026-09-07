//! Epoch-aligned tumbling windows and the aggregation pipeline.

use crate::domain::{
    Finite, Measurement, MeasurementType, ProcessingParameters, Statistics, Timestamp, Window,
};

use super::{StatsError, statistics};

/// The start of the tumbling window that contains `at`: windows are aligned
/// to the Unix epoch in UTC and are `window.duration()` wide, so a
/// 24-hour window starts at midnight UTC and a 7-day window on a Thursday
/// boundary counted from 1970-01-01. An instant within one window of year
/// 0000, whose floor is not representable, starts its own window at itself.
pub fn window_start(at: Timestamp, window: Window) -> Timestamp {
    let width = window.duration().as_nanos() as i128;
    let start = at.unix_nanos().div_euclid(width) * width;
    // Only an instant within one window of year 0000 has an unrepresentable
    // floor; it then starts its own window at itself, which keeps this
    // function total without a panic path.
    Timestamp::from_unix_nanos(start).unwrap_or(at)
}

/// One tumbling window's worth of items.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub struct WindowSlice<'a, T> {
    pub start: Timestamp,
    pub items: &'a [T],
}

/// Splits time-ordered `items` into consecutive tumbling windows of
/// `window`, yielding each window that holds at least one item. Items must
/// already be ordered by `at`; the split borrows the input, so no memory
/// grows with the number of windows.
///
/// The grouping is exactly "consecutive items with the same
/// [`window_start`]", but it constructs one timestamp per window rather
/// than one per item: because the items are ordered, an item shares the
/// first item's window while its instant lies before that window's end.
pub fn tumbling<'a, T>(
    items: &'a [T],
    window: Window,
    at: impl Fn(&T) -> Timestamp + 'a,
) -> impl Iterator<Item = WindowSlice<'a, T>> + 'a {
    let width = window.duration().as_nanos() as i128;
    let mut rest = items;
    std::iter::from_fn(move || {
        let first = at(rest.first()?);
        let first_nanos = first.unix_nanos();
        let floor = first_nanos.div_euclid(width) * width;
        // When the floor is representable the window is [floor, floor +
        // width). When it is not (within one window of year 0000),
        // `window_start` is the instant itself, so only items at exactly
        // the first instant share its window.
        let (start, end) = match Timestamp::from_unix_nanos(floor) {
            Ok(start) => (start, floor + width),
            Err(_) => (first, first_nanos + 1),
        };
        let len = rest
            .iter()
            .position(|item| at(item).unix_nanos() >= end)
            .unwrap_or(rest.len());
        let (head, tail) = rest.split_at(len);
        rest = tail;
        Some(WindowSlice { start, items: head })
    })
}

/// The statistics of one measurement type in one window.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct WindowStatistics {
    pub measurement_type: MeasurementType,
    pub window: Window,
    pub window_start: Timestamp,
    pub statistics: Statistics,
}

/// Runs the pipeline over `measurements`: time ordering, then for every
/// requested measurement type and window, the statistics of each window
/// that holds data. Measurements of types the parameters do not request are
/// ignored. Output is ordered by type, window and window start, so equal
/// inputs yield equal outputs.
pub fn aggregate(
    measurements: &mut [Measurement],
    parameters: &ProcessingParameters,
) -> Result<Vec<WindowStatistics>, StatsError> {
    super::time_order(measurements);
    let mut out = Vec::new();
    let mut values: Vec<Finite> = Vec::new();
    for &measurement_type in parameters.measurement_types() {
        let of_type: Vec<&Measurement> = measurements
            .iter()
            .filter(|m| m.measurement_type() == measurement_type)
            .collect();
        for &window in parameters.windows() {
            for slice in tumbling(&of_type, window, |m| m.recorded_at()) {
                values.clear();
                values.extend(slice.items.iter().map(|m| m.value().finite()));
                if let Some(statistics) = statistics(&values, parameters.percentiles())? {
                    out.push(WindowStatistics {
                        measurement_type,
                        window,
                        window_start: slice.start,
                        statistics,
                    });
                }
            }
        }
    }
    Ok(out)
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::domain::{MeasurementId, MeasurementValue, Percentile};

    fn ts(raw: &str) -> Timestamp {
        Timestamp::parse(raw).unwrap()
    }

    fn m(id: &str, kind: MeasurementType, value: f64, at: &str) -> Measurement {
        Measurement::new(
            MeasurementId::parse(id).unwrap(),
            kind,
            MeasurementValue::new(value).unwrap(),
            kind.canonical_unit(),
            ts(at),
        )
        .unwrap()
    }

    #[test]
    fn window_starts_are_epoch_aligned() {
        let at = ts("2026-09-06T12:34:56.789Z");
        assert_eq!(
            window_start(at, Window::OneMinute),
            ts("2026-09-06T12:34:00Z")
        );
        assert_eq!(
            window_start(at, Window::FiveMinutes),
            ts("2026-09-06T12:30:00Z")
        );
        assert_eq!(
            window_start(at, Window::FifteenMinutes),
            ts("2026-09-06T12:30:00Z")
        );
        assert_eq!(
            window_start(at, Window::OneHour),
            ts("2026-09-06T12:00:00Z")
        );
        assert_eq!(
            window_start(at, Window::SixHours),
            ts("2026-09-06T12:00:00Z")
        );
        assert_eq!(window_start(at, Window::OneDay), ts("2026-09-06T00:00:00Z"));
        // 7-day windows count from 1970-01-01 (a Thursday); 2026-09-03 is
        // the Thursday boundary before 2026-09-06.
        assert_eq!(
            window_start(at, Window::SevenDays),
            ts("2026-09-03T00:00:00Z")
        );
        // A boundary instant starts its own window.
        assert_eq!(
            window_start(ts("2026-09-06T12:00:00Z"), Window::OneHour),
            ts("2026-09-06T12:00:00Z")
        );
        // Pre-epoch instants floor towards the past, not towards zero.
        assert_eq!(
            window_start(ts("1969-12-31T23:59:59Z"), Window::OneHour),
            ts("1969-12-31T23:00:00Z")
        );
        // Year 0000 begins on a Saturday, so the 7-day floor of its first
        // second lies before year 0000 and is not representable: the instant
        // starts its own window.
        assert_eq!(
            window_start(ts("0000-01-01T00:00:01Z"), Window::SevenDays),
            ts("0000-01-01T00:00:01Z")
        );
    }

    #[test]
    fn tumbling_groups_consecutive_items_and_skips_gaps() {
        let items = [
            m(
                "a",
                MeasurementType::HeartRate,
                60.0,
                "2026-09-06T12:00:10Z",
            ),
            m(
                "b",
                MeasurementType::HeartRate,
                61.0,
                "2026-09-06T12:00:50Z",
            ),
            m(
                "c",
                MeasurementType::HeartRate,
                62.0,
                "2026-09-06T12:01:00Z",
            ),
            m(
                "d",
                MeasurementType::HeartRate,
                63.0,
                "2026-09-06T12:05:00Z",
            ),
        ];
        let windows: Vec<_> = tumbling(&items, Window::OneMinute, |m| m.recorded_at()).collect();
        assert_eq!(windows.len(), 3);
        assert_eq!(windows[0].start, ts("2026-09-06T12:00:00Z"));
        assert_eq!(windows[0].items.len(), 2);
        assert_eq!(windows[1].start, ts("2026-09-06T12:01:00Z"));
        assert_eq!(windows[1].items.len(), 1);
        assert_eq!(windows[2].start, ts("2026-09-06T12:05:00Z"));
        assert_eq!(windows[2].items.len(), 1);
        assert_eq!(
            tumbling(&[] as &[Measurement], Window::OneMinute, |m| m
                .recorded_at())
            .count(),
            0
        );
    }

    /// The grouping must equal "consecutive items with the same
    /// `window_start`" for every window, across the epoch and at the
    /// unrepresentable floor of year 0000.
    #[test]
    fn tumbling_agrees_with_window_start_per_item() {
        let mut nanos: Vec<i128> = Vec::new();
        let mut state: u64 = 0x9E37_79B9_7F4A_7C15;
        let mut t: i128 = ts("1969-12-25T00:00:00Z").unix_nanos();
        for _ in 0..5_000 {
            state ^= state << 13;
            state ^= state >> 7;
            state ^= state << 17;
            t += (state % 900_000_000_000) as i128; // up to 15 minutes
            nanos.push(t);
        }
        // Two items at the first instant of year 0000, then hourly items
        // for 400 hours, then the pseudo-random series around the epoch.
        let year_zero = ts("0000-01-01T00:00:00Z").unix_nanos();
        let mut all = vec![year_zero, year_zero];
        all.extend((1..400).map(|i| year_zero + i as i128 * 3_600_000_000_000));
        all.extend(nanos);
        let items: Vec<Timestamp> = all
            .into_iter()
            .map(|n| Timestamp::from_unix_nanos(n).unwrap())
            .collect();
        for window in Window::ALL {
            let mut expected: Vec<(Timestamp, usize)> = Vec::new();
            for &item in &items {
                let start = window_start(item, window);
                match expected.last_mut() {
                    Some((s, n)) if *s == start => *n += 1,
                    _ => expected.push((start, 1)),
                }
            }
            let got: Vec<(Timestamp, usize)> = tumbling(&items, window, |&t| t)
                .map(|w| (w.start, w.items.len()))
                .collect();
            assert_eq!(got, expected, "{window:?}");
        }
    }

    fn params(windows: &[Window], percentiles: &[u8]) -> ProcessingParameters {
        ProcessingParameters::new(
            [MeasurementType::HeartRate, MeasurementType::Spo2],
            windows.iter().copied(),
            percentiles.iter().map(|&p| Percentile::new(p).unwrap()),
        )
        .unwrap()
    }

    #[test]
    fn aggregate_produces_one_result_per_type_window_and_start() {
        let mut items = vec![
            m(
                "h3",
                MeasurementType::HeartRate,
                80.0,
                "2026-09-06T12:01:30Z",
            ),
            m(
                "h1",
                MeasurementType::HeartRate,
                60.0,
                "2026-09-06T12:00:10Z",
            ),
            m(
                "h2",
                MeasurementType::HeartRate,
                70.0,
                "2026-09-06T12:00:40Z",
            ),
            m("s1", MeasurementType::Spo2, 97.0, "2026-09-06T12:00:20Z"),
            m(
                "t1",
                MeasurementType::BodyTemperature,
                36.6,
                "2026-09-06T12:00:20Z",
            ),
        ];
        let out = aggregate(
            &mut items,
            &params(&[Window::OneMinute, Window::OneHour], &[50]),
        )
        .unwrap();
        let keys: Vec<(MeasurementType, Window, String)> = out
            .iter()
            .map(|w| (w.measurement_type, w.window, w.window_start.to_rfc3339()))
            .collect();
        assert_eq!(
            keys,
            [
                (
                    MeasurementType::HeartRate,
                    Window::OneMinute,
                    "2026-09-06T12:00:00Z".to_owned()
                ),
                (
                    MeasurementType::HeartRate,
                    Window::OneMinute,
                    "2026-09-06T12:01:00Z".to_owned()
                ),
                (
                    MeasurementType::HeartRate,
                    Window::OneHour,
                    "2026-09-06T12:00:00Z".to_owned()
                ),
                (
                    MeasurementType::Spo2,
                    Window::OneMinute,
                    "2026-09-06T12:00:00Z".to_owned()
                ),
                (
                    MeasurementType::Spo2,
                    Window::OneHour,
                    "2026-09-06T12:00:00Z".to_owned()
                ),
            ]
        );
        let first_minute = &out[0].statistics;
        assert_eq!(first_minute.count(), 2);
        assert_eq!(first_minute.mean().get(), 65.0);
        assert_eq!(first_minute.median().get(), 65.0);
        let hour = &out[2].statistics;
        assert_eq!(hour.count(), 3);
        assert_eq!(hour.min().get(), 60.0);
        assert_eq!(hour.max().get(), 80.0);
        assert_eq!(
            hour.percentiles()[&Percentile::new(50).unwrap()].get(),
            70.0
        );
        // The input was time-ordered in place.
        assert_eq!(items[0].id().as_str(), "h1");
    }

    #[test]
    fn aggregate_handles_empty_and_single_inputs() {
        let mut none: Vec<Measurement> = Vec::new();
        assert!(
            aggregate(&mut none, &params(&Window::ALL, &[90]))
                .unwrap()
                .is_empty()
        );

        let mut one = vec![m(
            "h1",
            MeasurementType::HeartRate,
            64.0,
            "2026-09-06T12:00:00Z",
        )];
        let out = aggregate(&mut one, &params(&Window::ALL, &[1, 99])).unwrap();
        assert_eq!(out.len(), Window::ALL.len());
        for w in &out {
            assert_eq!(w.statistics.count(), 1);
            assert_eq!(w.statistics.variance(), Finite::ZERO);
            assert_eq!(w.statistics.mean().get(), 64.0);
        }
        assert_eq!(out[0].window, Window::OneMinute);
        assert_eq!(out[6].window, Window::SevenDays);
        assert_eq!(out[6].window_start, ts("2026-09-03T00:00:00Z"));
    }

    #[test]
    fn aggregate_is_deterministic_across_input_orders() {
        let base: Vec<Measurement> = (0..500)
            .map(|i| {
                m(
                    &format!("m-{i:04}"),
                    MeasurementType::HeartRate,
                    55.0 + (i % 37) as f64 * 0.7,
                    &format!(
                        "2026-09-06T{:02}:{:02}:{:02}Z",
                        (i / 3600) % 24,
                        (i / 60) % 60,
                        i % 60
                    ),
                )
            })
            .collect();
        let mut forward = base.clone();
        let mut backward: Vec<_> = base.iter().rev().cloned().collect();
        let p = params(&Window::ALL, &[5, 50, 95]);
        let a = aggregate(&mut forward, &p).unwrap();
        let b = aggregate(&mut backward, &p).unwrap();
        assert_eq!(a, b);
        assert!(!a.is_empty());
    }
}
