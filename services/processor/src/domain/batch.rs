//! A batch of measurements for one patient, bounded in size and ordered
//! deterministically by recording time.

use std::num::NonZeroUsize;

use serde::{Deserialize, Serialize};

use super::error::DomainError;
use super::ids::PatientId;
use super::measurement::Measurement;
use super::timestamp::Timestamp;

/// Size limit applied when a batch is built.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub struct BatchLimits {
    pub max_size: NonZeroUsize,
}

/// A non-empty, size-bounded batch whose measurements are sorted by
/// `(recorded_at, id)`. The order is total, so two batches built from the
/// same measurements are identical regardless of input order.
#[derive(Debug, Clone, PartialEq, Eq, Serialize)]
pub struct MeasurementBatch {
    patient_id: PatientId,
    measurements: Vec<Measurement>,
}

impl MeasurementBatch {
    pub fn new(
        patient_id: PatientId,
        mut measurements: Vec<Measurement>,
        limits: BatchLimits,
    ) -> Result<Self, DomainError> {
        if measurements.is_empty() {
            return Err(DomainError::EmptyBatch);
        }
        if measurements.len() > limits.max_size.get() {
            return Err(DomainError::BatchTooLarge {
                size: measurements.len(),
                max: limits.max_size.get(),
            });
        }
        measurements.sort_by(|a, b| {
            a.recorded_at()
                .cmp(&b.recorded_at())
                .then_with(|| a.id().cmp(b.id()))
        });
        Ok(Self {
            patient_id,
            measurements,
        })
    }

    pub fn patient_id(&self) -> &PatientId {
        &self.patient_id
    }

    pub fn measurements(&self) -> &[Measurement] {
        &self.measurements
    }

    pub fn len(&self) -> usize {
        self.measurements.len()
    }

    /// Always false: a batch cannot be empty. Provided for API symmetry.
    pub fn is_empty(&self) -> bool {
        self.measurements.is_empty()
    }

    /// Recording times of the first and last measurement.
    pub fn span(&self) -> (Timestamp, Timestamp) {
        let first = self.measurements[0].recorded_at();
        let last = self.measurements[self.measurements.len() - 1].recorded_at();
        (first, last)
    }

    pub fn into_measurements(self) -> Vec<Measurement> {
        self.measurements
    }
}

/// The wire shape of a batch. Each measurement is validated during
/// deserialization; the batch-level rules need the configured limits, so
/// they are applied by [`MeasurementBatchInput::into_batch`].
#[derive(Debug, Clone, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct MeasurementBatchInput {
    pub patient_id: PatientId,
    pub measurements: Vec<Measurement>,
}

impl MeasurementBatchInput {
    pub fn into_batch(self, limits: BatchLimits) -> Result<MeasurementBatch, DomainError> {
        MeasurementBatch::new(self.patient_id, self.measurements, limits)
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::domain::{MeasurementId, MeasurementType, MeasurementValue, Unit};

    fn limits(n: usize) -> BatchLimits {
        BatchLimits {
            max_size: NonZeroUsize::new(n).unwrap(),
        }
    }

    fn measurement(id: &str, at: &str) -> Measurement {
        Measurement::new(
            MeasurementId::parse(id).unwrap(),
            MeasurementType::HeartRate,
            MeasurementValue::new(70.0).unwrap(),
            Unit::BeatsPerMinute,
            Timestamp::parse(at).unwrap(),
        )
        .unwrap()
    }

    fn patient() -> PatientId {
        PatientId::parse("p-1").unwrap()
    }

    #[test]
    fn sorts_by_time_then_id_regardless_of_input_order() {
        let a = measurement("b", "2026-09-06T12:00:00Z");
        let b = measurement("a", "2026-09-06T12:00:00Z");
        let c = measurement("c", "2026-09-06T11:59:59.999999999Z");

        let batch1 =
            MeasurementBatch::new(patient(), vec![a.clone(), b.clone(), c.clone()], limits(10))
                .unwrap();
        let batch2 =
            MeasurementBatch::new(patient(), vec![b.clone(), c.clone(), a.clone()], limits(10))
                .unwrap();

        assert_eq!(batch1, batch2);
        let ids: Vec<&str> = batch1
            .measurements()
            .iter()
            .map(|m| m.id().as_str())
            .collect();
        assert_eq!(ids, ["c", "a", "b"]);
        assert_eq!(batch1.len(), 3);
        assert!(!batch1.is_empty());
        assert_eq!(
            batch1.span(),
            (c.recorded_at(), a.recorded_at()),
            "span is first to last recording time"
        );
    }

    #[test]
    fn rejects_empty_and_oversized_batches() {
        assert_eq!(
            MeasurementBatch::new(patient(), vec![], limits(10)).unwrap_err(),
            DomainError::EmptyBatch
        );

        let three: Vec<Measurement> = (0..3)
            .map(|i| measurement(&format!("m{i}"), "2026-09-06T12:00:00Z"))
            .collect();
        assert!(
            MeasurementBatch::new(patient(), three.clone(), limits(3)).is_ok(),
            "limit is inclusive"
        );
        assert_eq!(
            MeasurementBatch::new(patient(), three, limits(2)).unwrap_err(),
            DomainError::BatchTooLarge { size: 3, max: 2 }
        );
    }

    #[test]
    fn input_deserialises_validated_measurements_then_applies_limits() {
        let json = r#"{"patient_id":"p-1","measurements":[
            {"id":"m-2","type":"SPO2","value":98,"unit":"%","recorded_at":"2026-09-06T12:00:01Z"},
            {"id":"m-1","type":"SPO2","value":97,"unit":"%","recorded_at":"2026-09-06T12:00:00Z"}
        ]}"#;
        let input: MeasurementBatchInput = serde_json::from_str(json).unwrap();
        let batch = input.clone().into_batch(limits(2)).unwrap();
        assert_eq!(batch.patient_id().as_str(), "p-1");
        assert_eq!(batch.measurements()[0].id().as_str(), "m-1");
        assert_eq!(
            input.into_batch(limits(1)).unwrap_err().code(),
            "BATCH_TOO_LARGE"
        );

        let invalid_inner = r#"{"patient_id":"p-1","measurements":[
            {"id":"m-1","type":"SPO2","value":101,"unit":"%","recorded_at":"2026-09-06T12:00:00Z"}
        ]}"#;
        let err = serde_json::from_str::<MeasurementBatchInput>(invalid_inner).unwrap_err();
        assert!(err.to_string().contains("technical range"), "{err}");

        let unknown = r#"{"patient_id":"p-1","measurements":[],"source":"x"}"#;
        assert!(serde_json::from_str::<MeasurementBatchInput>(unknown).is_err());
    }

    #[test]
    fn serialises_in_sorted_order() {
        let batch = MeasurementBatch::new(
            patient(),
            vec![
                measurement("z", "2026-09-06T12:00:05Z"),
                measurement("a", "2026-09-06T12:00:00Z"),
            ],
            limits(5),
        )
        .unwrap();
        let json = serde_json::to_string(&batch).unwrap();
        assert!(
            json.starts_with(r#"{"patient_id":"p-1","measurements":[{"id":"a""#),
            "{json}"
        );
        let round: MeasurementBatchInput = serde_json::from_str(&json).unwrap();
        assert_eq!(round.into_batch(limits(5)).unwrap(), batch);
    }
}
