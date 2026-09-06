//! Domain model of the processing service.
//!
//! Every type here is constructed through a validating constructor or a
//! validating `Deserialize` implementation, so a value that exists is a
//! valid one. Numbers are finite IEEE-754 binary64 values with a total
//! order; timestamps are UTC; identifiers are opaque validated strings.
//!
//! Module map:
//! - `error`: [`DomainError`], the reason a value was rejected.
//! - `ids`: [`JobId`], [`PatientId`], [`MeasurementId`].
//! - `value`: [`Finite`] and [`MeasurementValue`].
//! - `timestamp`: [`Timestamp`] and [`TimestampBounds`].
//! - `measurement`: [`MeasurementType`], [`Unit`], [`Measurement`].
//! - `batch`: [`MeasurementBatch`].
//! - `job`: [`JobStatus`], [`Window`], [`Percentile`], [`ProcessingParameters`], [`ProcessingJob`].
//! - `result`: [`Severity`], [`AnomalyMetric`], [`Anomaly`], [`Statistics`], [`ProcessingResult`].
//! - `version`: [`AlgorithmVersion`].

mod batch;
mod error;
mod ids;
mod job;
mod measurement;
mod result;
mod timestamp;
mod value;
mod version;

pub use batch::{BatchLimits, MeasurementBatch, MeasurementBatchInput};
pub use error::DomainError;
pub use ids::{JobId, MeasurementId, PatientId};
pub use job::{JobStatus, Percentile, ProcessingJob, ProcessingParameters, Window};
pub use measurement::{Measurement, MeasurementType, Unit, ValueRange};
pub use result::{
    Anomaly, AnomalyMetric, ProcessingResult, ProcessingResultInput, Severity, Statistics,
    StatisticsInput,
};
pub use timestamp::{Timestamp, TimestampBounds};
pub use value::{Finite, MeasurementValue};
pub use version::AlgorithmVersion;
