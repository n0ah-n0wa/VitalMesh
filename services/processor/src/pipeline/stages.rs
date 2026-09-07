//! The validation and normalisation stages.

use std::collections::HashMap;
use std::collections::hash_map::Entry;

use serde::Serialize;

use crate::domain::{
    Measurement, MeasurementId, MeasurementType, MeasurementValue, ProcessingParameters, Timestamp,
    TimestampBounds, Unit,
};

use super::RawReading;

/// Why a reading was rejected. Codes are stable and part of the contract.
#[derive(Debug, Clone, Copy, PartialEq, Eq, Serialize)]
#[serde(rename_all = "SCREAMING_SNAKE_CASE")]
pub enum RejectionCode {
    InvalidId,
    UnknownType,
    UnknownUnit,
    UnitMismatch,
    NonFiniteValue,
    ValueOutOfRange,
    InvalidTimestamp,
    TimestampOutOfBounds,
    DuplicateId,
}

/// One rejected reading: its input index, its identifier when it had a
/// usable one, and the reason.
#[derive(Debug, Clone, PartialEq, Eq, Serialize)]
pub struct Rejection {
    pub index: usize,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub id: Option<String>,
    pub code: RejectionCode,
    pub reason: String,
}

/// A validated reading with the input index it came from.
pub(super) struct Validated {
    pub(super) index: usize,
    pub(super) measurement: Measurement,
}

fn parse_type(raw: &str) -> Option<MeasurementType> {
    MeasurementType::ALL.into_iter().find(|t| t.as_str() == raw)
}

fn parse_unit(raw: &str) -> Option<Unit> {
    [
        Unit::BeatsPerMinute,
        Unit::MillimetresOfMercury,
        Unit::Percent,
        Unit::Celsius,
        Unit::MilligramsPerDecilitre,
        Unit::BreathsPerMinute,
    ]
    .into_iter()
    .find(|u| u.symbol() == raw)
}

/// Validates every reading against the domain rules and the timestamp
/// bounds relative to `now` (the job's request time, so that the outcome
/// does not depend on when the job runs). Each reading is judged on its
/// own; the first failing rule is reported. The readings are consumed so
/// that an accepted identifier keeps its allocation.
pub(super) fn validate(
    readings: Vec<RawReading>,
    bounds: &TimestampBounds,
    now: Timestamp,
) -> (Vec<Validated>, Vec<Rejection>) {
    let mut accepted = Vec::with_capacity(readings.len());
    let mut rejected = Vec::new();
    for (index, raw) in readings.into_iter().enumerate() {
        match validate_one(raw, bounds, now) {
            Ok(measurement) => accepted.push(Validated { index, measurement }),
            Err(Rejected { id, code, reason }) => rejected.push(Rejection {
                index,
                id: id.map(|id| id.as_str().to_owned()),
                code,
                reason,
            }),
        }
    }
    (accepted, rejected)
}

/// A failed reading: the identifier is kept when it was usable, so that
/// the rejection can echo it.
struct Rejected {
    id: Option<MeasurementId>,
    code: RejectionCode,
    reason: String,
}

fn validate_one(
    raw: RawReading,
    bounds: &TimestampBounds,
    now: Timestamp,
) -> std::result::Result<Measurement, Rejected> {
    let id = MeasurementId::from_string(raw.id).map_err(|e| Rejected {
        id: None,
        code: RejectionCode::InvalidId,
        reason: e.to_string(),
    })?;
    let fields = Fields {
        measurement_type: &raw.measurement_type,
        value: raw.value,
        unit: &raw.unit,
        recorded_at: &raw.recorded_at,
    };
    let (measurement_type, value, unit, recorded_at) = match validate_fields(fields, bounds, now) {
        Ok(parsed) => parsed,
        Err((code, reason)) => {
            return Err(Rejected {
                id: Some(id),
                code,
                reason,
            });
        }
    };
    // The domain enforces the unit and the range; the same two checks here
    // only decide whether to keep a copy of the identifier for the echo, so
    // the common path constructs the measurement without an allocation.
    let echo = (measurement_type.canonical_unit() != unit
        || !measurement_type.value_range().contains(value))
    .then(|| id.clone());
    Measurement::new(id, measurement_type, value, unit, recorded_at).map_err(|e| {
        let code = match e.code() {
            "UNIT_MISMATCH" => RejectionCode::UnitMismatch,
            "VALUE_OUT_OF_RANGE" => RejectionCode::ValueOutOfRange,
            _ => RejectionCode::NonFiniteValue,
        };
        Rejected {
            id: echo,
            code,
            reason: e.to_string(),
        }
    })
}

/// The fields of a reading other than its identifier, borrowed.
struct Fields<'a> {
    measurement_type: &'a str,
    value: f64,
    unit: &'a str,
    recorded_at: &'a str,
}

type Parsed = (MeasurementType, MeasurementValue, Unit, Timestamp);

fn validate_fields(
    raw: Fields<'_>,
    bounds: &TimestampBounds,
    now: Timestamp,
) -> std::result::Result<Parsed, (RejectionCode, String)> {
    let measurement_type = parse_type(raw.measurement_type).ok_or_else(|| {
        (
            RejectionCode::UnknownType,
            format!("unknown measurement type {:?}", raw.measurement_type),
        )
    })?;
    let unit = parse_unit(raw.unit).ok_or_else(|| {
        (
            RejectionCode::UnknownUnit,
            format!("unknown unit {:?}", raw.unit),
        )
    })?;
    let value = MeasurementValue::new(raw.value)
        .map_err(|e| (RejectionCode::NonFiniteValue, e.to_string()))?;
    let recorded_at = Timestamp::parse(raw.recorded_at)
        .map_err(|e| (RejectionCode::InvalidTimestamp, e.to_string()))?;
    bounds
        .check(recorded_at, now)
        .map_err(|e| (RejectionCode::TimestampOutOfBounds, e.to_string()))?;
    Ok((measurement_type, value, unit, recorded_at))
}

/// Normalises validated readings: readings whose identifier appears more
/// than once are all rejected (a duplicate identifier means the input is
/// inconsistent and no occurrence can be trusted), and readings of types
/// the job did not request are dropped and counted. Values and timestamps
/// were normalised on construction (`-0.0` to `0.0`, offsets to UTC).
/// Accepted measurements are moved, not copied.
pub(super) fn normalize(
    validated: Vec<Validated>,
    parameters: &ProcessingParameters,
    rejected: &mut Vec<Rejection>,
) -> (Vec<Measurement>, usize) {
    let mut duplicate = vec![false; validated.len()];
    {
        let mut first_seen: HashMap<&str, usize> = HashMap::with_capacity(validated.len());
        for (i, v) in validated.iter().enumerate() {
            match first_seen.entry(v.measurement.id().as_str()) {
                Entry::Occupied(first) => {
                    duplicate[*first.get()] = true;
                    duplicate[i] = true;
                }
                Entry::Vacant(slot) => {
                    slot.insert(i);
                }
            }
        }
    }
    let mut measurements = Vec::with_capacity(validated.len());
    let mut skipped = 0;
    for (i, v) in validated.into_iter().enumerate() {
        if duplicate[i] {
            rejected.push(Rejection {
                index: v.index,
                id: Some(v.measurement.id().as_str().to_owned()),
                code: RejectionCode::DuplicateId,
                reason: "the identifier appears more than once in the job".to_owned(),
            });
            continue;
        }
        if !parameters
            .measurement_types()
            .contains(&v.measurement.measurement_type())
        {
            skipped += 1;
            continue;
        }
        measurements.push(v.measurement);
    }
    (measurements, skipped)
}

#[cfg(test)]
mod tests {
    use std::time::Duration;

    use super::*;
    use crate::domain::Window;

    fn bounds() -> TimestampBounds {
        TimestampBounds {
            max_future_skew: Duration::from_secs(300),
            max_age: Duration::from_secs(7 * 24 * 3600),
        }
    }

    fn now() -> Timestamp {
        Timestamp::parse("2026-09-06T12:00:00Z").unwrap()
    }

    fn raw(id: &str, kind: &str, value: f64, unit: &str, at: &str) -> RawReading {
        RawReading {
            id: id.into(),
            measurement_type: kind.into(),
            value,
            unit: unit.into(),
            recorded_at: at.into(),
        }
    }

    #[test]
    fn each_reading_is_judged_on_its_own_with_a_stable_code() {
        let readings = vec![
            raw("ok", "HEART_RATE", 72.0, "bpm", "2026-09-06T11:00:00Z"),
            raw("bad id!", "HEART_RATE", 72.0, "bpm", "2026-09-06T11:00:00Z"),
            raw("t", "PULSE", 72.0, "bpm", "2026-09-06T11:00:00Z"),
            raw("u", "HEART_RATE", 72.0, "beats", "2026-09-06T11:00:00Z"),
            raw("m", "HEART_RATE", 72.0, "mmHg", "2026-09-06T11:00:00Z"),
            raw("r", "HEART_RATE", 301.0, "bpm", "2026-09-06T11:00:00Z"),
            raw("n", "HEART_RATE", f64::NAN, "bpm", "2026-09-06T11:00:00Z"),
            raw("ts", "HEART_RATE", 72.0, "bpm", "yesterday"),
            raw("fut", "HEART_RATE", 72.0, "bpm", "2026-09-06T12:06:00Z"),
            raw("old", "HEART_RATE", 72.0, "bpm", "2026-08-01T00:00:00Z"),
            raw("edge", "HEART_RATE", 72.0, "bpm", "2026-09-06T12:05:00Z"),
        ];
        let (accepted, rejected) = validate(readings, &bounds(), now());
        let ids: Vec<&str> = accepted
            .iter()
            .map(|v| v.measurement.id().as_str())
            .collect();
        assert_eq!(ids, ["ok", "edge"], "the future-skew bound is inclusive");
        let codes: Vec<(usize, RejectionCode)> =
            rejected.iter().map(|r| (r.index, r.code)).collect();
        assert_eq!(
            codes,
            [
                (1, RejectionCode::InvalidId),
                (2, RejectionCode::UnknownType),
                (3, RejectionCode::UnknownUnit),
                (4, RejectionCode::UnitMismatch),
                (5, RejectionCode::ValueOutOfRange),
                (6, RejectionCode::NonFiniteValue),
                (7, RejectionCode::InvalidTimestamp),
                (8, RejectionCode::TimestampOutOfBounds),
                (9, RejectionCode::TimestampOutOfBounds),
            ]
        );
        assert_eq!(rejected[0].id, None, "an unusable id is not echoed");
        assert_eq!(rejected[1].id.as_deref(), Some("t"));
        assert!(rejected.iter().all(|r| !r.reason.is_empty()));
    }

    #[test]
    fn normalisation_rejects_every_occurrence_of_a_duplicate_id_and_skips_unrequested_types() {
        let readings = vec![
            raw("a", "HEART_RATE", 70.0, "bpm", "2026-09-06T11:00:00Z"),
            raw("dup", "HEART_RATE", 71.0, "bpm", "2026-09-06T11:00:01Z"),
            raw("s", "SPO2", 97.0, "%", "2026-09-06T11:00:02Z"),
            raw("dup", "HEART_RATE", 72.0, "bpm", "2026-09-06T11:00:03Z"),
            raw("dup", "SPO2", 95.0, "%", "2026-09-06T11:00:04Z"),
        ];
        let (validated, mut rejected) = validate(readings, &bounds(), now());
        assert!(rejected.is_empty());
        let parameters =
            ProcessingParameters::new([MeasurementType::HeartRate], [Window::OneHour], []).unwrap();
        let (measurements, skipped) = normalize(validated, &parameters, &mut rejected);
        let ids: Vec<&str> = measurements.iter().map(|m| m.id().as_str()).collect();
        assert_eq!(ids, ["a"]);
        assert_eq!(skipped, 1, "the SPO2 reading is skipped, not rejected");
        let dup_indexes: Vec<usize> = rejected.iter().map(|r| r.index).collect();
        assert_eq!(dup_indexes, [1, 3, 4]);
        assert!(
            rejected
                .iter()
                .all(|r| r.code == RejectionCode::DuplicateId)
        );
    }

    #[test]
    fn empty_input_validates_to_nothing() {
        let (accepted, rejected) = validate(Vec::new(), &bounds(), now());
        assert!(accepted.is_empty() && rejected.is_empty());
    }
}
