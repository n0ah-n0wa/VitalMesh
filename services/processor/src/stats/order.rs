//! Time ordering: the "Time Ordering" step of the pipeline.

use crate::domain::Measurement;

/// Sorts measurements by `recorded_at`, breaking ties by identifier, so the
/// order is total and identical for identical input sets whatever order
/// they arrived in. The sort is stable; identifiers are unique per patient,
/// so stability only matters for exact duplicates, which keep arrival order.
pub fn time_order(measurements: &mut [Measurement]) {
    measurements.sort_by(|a, b| {
        a.recorded_at()
            .cmp(&b.recorded_at())
            .then_with(|| a.id().as_str().cmp(b.id().as_str()))
    });
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::domain::{MeasurementId, MeasurementType, MeasurementValue, Timestamp};

    fn m(id: &str, at: &str) -> Measurement {
        Measurement::new(
            MeasurementId::parse(id).unwrap(),
            MeasurementType::HeartRate,
            MeasurementValue::new(70.0).unwrap(),
            MeasurementType::HeartRate.canonical_unit(),
            Timestamp::parse(at).unwrap(),
        )
        .unwrap()
    }

    #[test]
    fn orders_by_time_then_identifier() {
        let mut items = vec![
            m("m-3", "2026-09-06T12:00:02Z"),
            m("m-2", "2026-09-06T12:00:01Z"),
            m("m-1", "2026-09-06T12:00:01Z"),
            m("m-0", "2026-09-06T11:59:59Z"),
        ];
        time_order(&mut items);
        let ids: Vec<&str> = items.iter().map(|m| m.id().as_str()).collect();
        assert_eq!(ids, ["m-0", "m-1", "m-2", "m-3"]);
    }

    #[test]
    fn is_independent_of_arrival_order() {
        let a = vec![
            m("b", "2026-09-06T12:00:00Z"),
            m("a", "2026-09-06T12:00:00Z"),
            m("c", "2026-09-06T11:00:00Z"),
        ];
        let mut first = a.clone();
        let mut second: Vec<_> = a.into_iter().rev().collect();
        time_order(&mut first);
        time_order(&mut second);
        assert_eq!(first, second);
    }

    #[test]
    fn handles_empty_and_single() {
        let mut none: Vec<Measurement> = Vec::new();
        time_order(&mut none);
        assert!(none.is_empty());
        let mut one = vec![m("m", "2026-09-06T12:00:00Z")];
        time_order(&mut one);
        assert_eq!(one.len(), 1);
    }
}
