//! Measurement types, units and validated measurements.
//!
//! Each type has one canonical unit and a technical plausibility range. The
//! ranges reject data that cannot be a reading at all; they are input
//! validation, not medical thresholds.

use serde::{Deserialize, Serialize};

use super::error::DomainError;
use super::ids::MeasurementId;
use super::timestamp::Timestamp;
use super::value::MeasurementValue;

/// The kinds of measurement the platform accepts.
#[derive(Debug, Clone, Copy, PartialEq, Eq, Hash, PartialOrd, Ord, Serialize, Deserialize)]
#[serde(rename_all = "SCREAMING_SNAKE_CASE")]
pub enum MeasurementType {
    HeartRate,
    BloodPressureSystolic,
    BloodPressureDiastolic,
    Spo2,
    BodyTemperature,
    BloodGlucose,
    RespiratoryRate,
}

/// Inclusive technical bounds on a measurement value.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub struct ValueRange {
    pub min: MeasurementValue,
    pub max: MeasurementValue,
}

impl ValueRange {
    const fn new(min: f64, max: f64) -> Self {
        Self {
            min: MeasurementValue::literal(min),
            max: MeasurementValue::literal(max),
        }
    }

    pub fn contains(&self, value: MeasurementValue) -> bool {
        self.min <= value && value <= self.max
    }
}

impl MeasurementType {
    pub const ALL: [Self; 7] = [
        Self::HeartRate,
        Self::BloodPressureSystolic,
        Self::BloodPressureDiastolic,
        Self::Spo2,
        Self::BodyTemperature,
        Self::BloodGlucose,
        Self::RespiratoryRate,
    ];

    /// The wire name.
    pub fn as_str(self) -> &'static str {
        match self {
            Self::HeartRate => "HEART_RATE",
            Self::BloodPressureSystolic => "BLOOD_PRESSURE_SYSTOLIC",
            Self::BloodPressureDiastolic => "BLOOD_PRESSURE_DIASTOLIC",
            Self::Spo2 => "SPO2",
            Self::BodyTemperature => "BODY_TEMPERATURE",
            Self::BloodGlucose => "BLOOD_GLUCOSE",
            Self::RespiratoryRate => "RESPIRATORY_RATE",
        }
    }

    /// The only unit accepted for this type.
    pub fn canonical_unit(self) -> Unit {
        match self {
            Self::HeartRate => Unit::BeatsPerMinute,
            Self::BloodPressureSystolic | Self::BloodPressureDiastolic => {
                Unit::MillimetresOfMercury
            }
            Self::Spo2 => Unit::Percent,
            Self::BodyTemperature => Unit::Celsius,
            Self::BloodGlucose => Unit::MilligramsPerDecilitre,
            Self::RespiratoryRate => Unit::BreathsPerMinute,
        }
    }

    /// Technical plausibility bounds, inclusive.
    pub fn value_range(self) -> ValueRange {
        match self {
            Self::HeartRate => ValueRange::new(0.0, 300.0),
            Self::BloodPressureSystolic => ValueRange::new(0.0, 300.0),
            Self::BloodPressureDiastolic => ValueRange::new(0.0, 200.0),
            Self::Spo2 => ValueRange::new(0.0, 100.0),
            Self::BodyTemperature => ValueRange::new(20.0, 45.0),
            Self::BloodGlucose => ValueRange::new(0.0, 1000.0),
            Self::RespiratoryRate => ValueRange::new(0.0, 100.0),
        }
    }
}

/// Units of measurement, serialised by their symbol.
#[derive(Debug, Clone, Copy, PartialEq, Eq, Hash, Serialize, Deserialize)]
pub enum Unit {
    #[serde(rename = "bpm")]
    BeatsPerMinute,
    #[serde(rename = "mmHg")]
    MillimetresOfMercury,
    #[serde(rename = "%")]
    Percent,
    #[serde(rename = "C")]
    Celsius,
    #[serde(rename = "mg/dL")]
    MilligramsPerDecilitre,
    #[serde(rename = "breaths/min")]
    BreathsPerMinute,
}

impl Unit {
    /// The wire symbol.
    pub fn symbol(self) -> &'static str {
        match self {
            Self::BeatsPerMinute => "bpm",
            Self::MillimetresOfMercury => "mmHg",
            Self::Percent => "%",
            Self::Celsius => "C",
            Self::MilligramsPerDecilitre => "mg/dL",
            Self::BreathsPerMinute => "breaths/min",
        }
    }
}

/// A validated measurement: the unit matches the type and the value lies in
/// the type's technical range. Fields are private so no other construction
/// path exists.
#[derive(Debug, Clone, PartialEq, Eq, Serialize, Deserialize)]
#[serde(try_from = "RawMeasurement")]
pub struct Measurement {
    id: MeasurementId,
    #[serde(rename = "type")]
    measurement_type: MeasurementType,
    value: MeasurementValue,
    unit: Unit,
    recorded_at: Timestamp,
}

impl Measurement {
    pub fn new(
        id: MeasurementId,
        measurement_type: MeasurementType,
        value: MeasurementValue,
        unit: Unit,
        recorded_at: Timestamp,
    ) -> Result<Self, DomainError> {
        let expected = measurement_type.canonical_unit();
        if unit != expected {
            return Err(DomainError::UnitMismatch {
                measurement_type: measurement_type.as_str(),
                expected: expected.symbol(),
                actual: unit.symbol(),
            });
        }
        let range = measurement_type.value_range();
        if !range.contains(value) {
            return Err(DomainError::ValueOutOfRange {
                measurement_type: measurement_type.as_str(),
                value: value.to_string(),
                min: range.min.to_string(),
                max: range.max.to_string(),
            });
        }
        Ok(Self {
            id,
            measurement_type,
            value,
            unit,
            recorded_at,
        })
    }

    pub fn id(&self) -> &MeasurementId {
        &self.id
    }

    pub fn measurement_type(&self) -> MeasurementType {
        self.measurement_type
    }

    pub fn value(&self) -> MeasurementValue {
        self.value
    }

    pub fn unit(&self) -> Unit {
        self.unit
    }

    pub fn recorded_at(&self) -> Timestamp {
        self.recorded_at
    }
}

/// The wire shape of a measurement before validation.
#[derive(Debug, Deserialize)]
#[serde(deny_unknown_fields)]
struct RawMeasurement {
    id: MeasurementId,
    #[serde(rename = "type")]
    measurement_type: MeasurementType,
    value: MeasurementValue,
    unit: Unit,
    recorded_at: Timestamp,
}

impl TryFrom<RawMeasurement> for Measurement {
    type Error = DomainError;

    fn try_from(raw: RawMeasurement) -> Result<Self, DomainError> {
        Self::new(
            raw.id,
            raw.measurement_type,
            raw.value,
            raw.unit,
            raw.recorded_at,
        )
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    fn id() -> MeasurementId {
        MeasurementId::parse("m-1").unwrap()
    }

    fn at() -> Timestamp {
        Timestamp::parse("2026-09-06T12:00:00Z").unwrap()
    }

    fn value(v: f64) -> MeasurementValue {
        MeasurementValue::new(v).unwrap()
    }

    #[test]
    fn every_type_has_a_unit_a_sane_range_and_a_wire_name() {
        for kind in MeasurementType::ALL {
            let range = kind.value_range();
            assert!(range.min < range.max, "{kind:?}");
            assert_eq!(
                serde_json::to_string(&kind).unwrap(),
                format!("\"{}\"", kind.as_str())
            );
            let back: MeasurementType =
                serde_json::from_str(&format!("\"{}\"", kind.as_str())).unwrap();
            assert_eq!(back, kind);
            let unit = kind.canonical_unit();
            assert_eq!(
                serde_json::to_string(&unit).unwrap(),
                format!("\"{}\"", unit.symbol())
            );
        }
        assert_eq!(MeasurementType::Spo2.as_str(), "SPO2");
        assert_eq!(
            MeasurementType::BloodGlucose.canonical_unit(),
            Unit::MilligramsPerDecilitre
        );
    }

    #[test]
    fn unit_symbols_round_trip_including_special_characters() {
        for unit in [
            Unit::BeatsPerMinute,
            Unit::MillimetresOfMercury,
            Unit::Percent,
            Unit::Celsius,
            Unit::MilligramsPerDecilitre,
            Unit::BreathsPerMinute,
        ] {
            let json = serde_json::to_string(&unit).unwrap();
            assert_eq!(serde_json::from_str::<Unit>(&json).unwrap(), unit, "{json}");
        }
        assert_eq!(
            serde_json::from_str::<Unit>("\"mg/dL\"").unwrap(),
            Unit::MilligramsPerDecilitre
        );
        assert!(serde_json::from_str::<Unit>("\"F\"").is_err());
        assert!(
            serde_json::from_str::<Unit>("\"MG/DL\"").is_err(),
            "symbols are case-sensitive"
        );
    }

    #[test]
    fn invalid_enum_values_are_rejected_with_the_variant_name() {
        let err = serde_json::from_str::<MeasurementType>("\"heart_rate\"").unwrap_err();
        assert!(err.to_string().contains("unknown variant"), "{err}");
        assert!(serde_json::from_str::<MeasurementType>("\"PULSE\"").is_err());
        assert!(serde_json::from_str::<MeasurementType>("1").is_err());
    }

    #[test]
    fn accepts_values_at_the_range_boundaries() {
        for kind in MeasurementType::ALL {
            let range = kind.value_range();
            let unit = kind.canonical_unit();
            for v in [range.min, range.max] {
                Measurement::new(id(), kind, v, unit, at())
                    .unwrap_or_else(|e| panic!("{kind:?} {v}: {e}"));
            }
        }
        let m = Measurement::new(
            id(),
            MeasurementType::BodyTemperature,
            value(36.6),
            Unit::Celsius,
            at(),
        )
        .unwrap();
        assert_eq!(m.value().get(), 36.6);
        assert_eq!(m.unit(), Unit::Celsius);
        assert_eq!(m.recorded_at(), at());
        assert_eq!(m.id().as_str(), "m-1");
    }

    #[test]
    fn rejects_values_just_outside_the_range() {
        let too_high = Measurement::new(
            id(),
            MeasurementType::Spo2,
            value(100.0000001),
            Unit::Percent,
            at(),
        )
        .unwrap_err();
        assert_eq!(
            too_high,
            DomainError::ValueOutOfRange {
                measurement_type: "SPO2",
                value: "100.0000001".into(),
                min: "0".into(),
                max: "100".into(),
            }
        );
        let too_low = Measurement::new(
            id(),
            MeasurementType::BodyTemperature,
            value(19.999),
            Unit::Celsius,
            at(),
        )
        .unwrap_err();
        assert_eq!(too_low.code(), "VALUE_OUT_OF_RANGE");
        let negative = Measurement::new(
            id(),
            MeasurementType::HeartRate,
            value(-1.0),
            Unit::BeatsPerMinute,
            at(),
        )
        .unwrap_err();
        assert!(negative.to_string().contains("technical range"));
    }

    #[test]
    fn rejects_unit_mismatches() {
        let err = Measurement::new(
            id(),
            MeasurementType::BodyTemperature,
            value(98.6),
            Unit::Percent,
            at(),
        )
        .unwrap_err();
        assert_eq!(
            err,
            DomainError::UnitMismatch {
                measurement_type: "BODY_TEMPERATURE",
                expected: "C",
                actual: "%",
            }
        );
    }

    #[test]
    fn serde_round_trip_uses_the_wire_field_names() {
        let m = Measurement::new(
            id(),
            MeasurementType::HeartRate,
            value(72.0),
            Unit::BeatsPerMinute,
            at(),
        )
        .unwrap();
        let json = serde_json::to_string(&m).unwrap();
        assert_eq!(
            json,
            r#"{"id":"m-1","type":"HEART_RATE","value":72.0,"unit":"bpm","recorded_at":"2026-09-06T12:00:00Z"}"#
        );
        assert_eq!(serde_json::from_str::<Measurement>(&json).unwrap(), m);
    }

    #[test]
    fn deserialization_validates_at_the_boundary() {
        let cases = [
            (
                r#"{"id":"m-1","type":"HEART_RATE","value":72,"unit":"mmHg","recorded_at":"2026-09-06T12:00:00Z"}"#,
                "must use unit bpm",
            ),
            (
                r#"{"id":"m-1","type":"SPO2","value":101,"unit":"%","recorded_at":"2026-09-06T12:00:00Z"}"#,
                "technical range",
            ),
            (
                r#"{"id":"m-1","type":"SPO2","value":"98","unit":"%","recorded_at":"2026-09-06T12:00:00Z"}"#,
                "invalid type",
            ),
            (
                r#"{"id":"m 1","type":"SPO2","value":98,"unit":"%","recorded_at":"2026-09-06T12:00:00Z"}"#,
                "invalid measurement id",
            ),
            (
                r#"{"id":"m-1","type":"SPO2","value":98,"unit":"%","recorded_at":"2026-09-06"}"#,
                "invalid timestamp",
            ),
            (
                r#"{"id":"m-1","type":"SPO2","value":98,"unit":"%","recorded_at":"2026-09-06T12:00:00Z","extra":1}"#,
                "unknown field",
            ),
            (
                r#"{"id":"m-1","type":"SPO2","value":98,"unit":"%"}"#,
                "missing field",
            ),
        ];
        for (json, want) in cases {
            let err = serde_json::from_str::<Measurement>(json).expect_err(json);
            assert!(
                err.to_string().contains(want),
                "{json}\n  got: {err}\n  want: {want}"
            );
        }
    }
}
