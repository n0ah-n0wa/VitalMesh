//! Numeric values.
//!
//! Numbers are IEEE-754 binary64. The domain never holds NaN or infinity, so
//! every value has a total order and exact equality; negative zero is
//! normalised to zero so that `0.0 == -0.0` holds bit-for-bit. JSON
//! round-trips are exact because serde_json emits the shortest
//! representation that parses back to the same double.

use std::cmp::Ordering;
use std::fmt;
use std::hash::{Hash, Hasher};

use serde::{Deserialize, Deserializer, Serialize, Serializer};

use super::error::DomainError;

/// A finite double with a total order.
#[derive(Debug, Clone, Copy)]
pub struct Finite(f64);

impl Finite {
    pub const ZERO: Self = Self(0.0);

    /// Rejects NaN and infinities.
    pub fn new(value: f64) -> Result<Self, DomainError> {
        if value.is_finite() {
            Ok(Self::normalised(value))
        } else {
            Err(DomainError::NonFiniteValue {
                value: value.to_string(),
            })
        }
    }

    /// For compile-time literals only; the caller guarantees finiteness.
    pub(crate) const fn literal(value: f64) -> Self {
        Self::normalised(value)
    }

    const fn normalised(value: f64) -> Self {
        // -0.0 == 0.0 but differs bitwise; keep a single representation.
        if value == 0.0 { Self(0.0) } else { Self(value) }
    }

    pub fn get(self) -> f64 {
        self.0
    }
}

impl PartialEq for Finite {
    fn eq(&self, other: &Self) -> bool {
        self.0.to_bits() == other.0.to_bits()
    }
}

impl Eq for Finite {}

impl PartialOrd for Finite {
    fn partial_cmp(&self, other: &Self) -> Option<Ordering> {
        Some(self.cmp(other))
    }
}

impl Ord for Finite {
    fn cmp(&self, other: &Self) -> Ordering {
        self.0.total_cmp(&other.0)
    }
}

impl Hash for Finite {
    fn hash<H: Hasher>(&self, state: &mut H) {
        self.0.to_bits().hash(state);
    }
}

impl fmt::Display for Finite {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        fmt::Display::fmt(&self.0, f)
    }
}

impl Serialize for Finite {
    fn serialize<S: Serializer>(&self, serializer: S) -> Result<S::Ok, S::Error> {
        serializer.serialize_f64(self.0)
    }
}

impl<'de> Deserialize<'de> for Finite {
    fn deserialize<D: Deserializer<'de>>(deserializer: D) -> Result<Self, D::Error> {
        let raw = f64::deserialize(deserializer)?;
        Self::new(raw).map_err(serde::de::Error::custom)
    }
}

/// The numeric value of one measurement, in the measurement type's
/// canonical unit. Range checks depend on the type and happen when a
/// [`super::Measurement`] is built.
#[derive(Debug, Clone, Copy, PartialEq, Eq, PartialOrd, Ord, Hash, Serialize, Deserialize)]
#[serde(transparent)]
pub struct MeasurementValue(Finite);

impl MeasurementValue {
    pub fn new(value: f64) -> Result<Self, DomainError> {
        Finite::new(value).map(Self)
    }

    pub(crate) const fn literal(value: f64) -> Self {
        Self(Finite::literal(value))
    }

    pub fn finite(self) -> Finite {
        self.0
    }

    pub fn get(self) -> f64 {
        self.0.get()
    }
}

impl From<Finite> for MeasurementValue {
    fn from(value: Finite) -> Self {
        Self(value)
    }
}

impl fmt::Display for MeasurementValue {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        fmt::Display::fmt(&self.0, f)
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn accepts_finite_values_and_rejects_the_rest() {
        for v in [
            0.0,
            -0.0,
            1.5,
            -273.15,
            f64::MAX,
            f64::MIN,
            f64::MIN_POSITIVE,
            5e-324,
        ] {
            assert_eq!(Finite::new(v).unwrap().get(), v);
        }
        for v in [f64::NAN, f64::INFINITY, f64::NEG_INFINITY] {
            let err = Finite::new(v).unwrap_err();
            assert_eq!(err.code(), "NON_FINITE_VALUE");
            assert!(MeasurementValue::new(v).is_err());
        }
    }

    #[test]
    fn negative_zero_is_normalised() {
        let neg = Finite::new(-0.0).unwrap();
        let pos = Finite::new(0.0).unwrap();
        assert_eq!(neg, pos);
        assert!(neg.get().is_sign_positive());
        assert_eq!(neg.cmp(&pos), Ordering::Equal);
        assert_eq!(serde_json::to_string(&neg).unwrap(), "0.0");
    }

    #[test]
    fn ordering_is_total_and_hashing_is_consistent() {
        let mut values: Vec<Finite> = [3.0, -1.0, 2.5, 0.0, -1e300, 1e300]
            .into_iter()
            .map(|v| Finite::new(v).unwrap())
            .collect();
        values.sort();
        let sorted: Vec<f64> = values.iter().map(|v| v.get()).collect();
        assert_eq!(sorted, [-1e300, -1.0, 0.0, 2.5, 3.0, 1e300]);

        use std::collections::HashSet;
        let set: HashSet<Finite> = [1.0, 1.0, 2.0]
            .into_iter()
            .map(|v| Finite::new(v).unwrap())
            .collect();
        assert_eq!(set.len(), 2);
    }

    #[test]
    fn serde_round_trip_is_exact() {
        for v in [
            0.1,
            1.0 / 3.0,
            36.6,
            98.25,
            1e-7,
            123456789.12345679,
            f64::MAX,
        ] {
            let finite = Finite::new(v).unwrap();
            let json = serde_json::to_string(&finite).unwrap();
            let back: Finite = serde_json::from_str(&json).unwrap();
            assert_eq!(back, finite, "{json}");
        }
        let value: MeasurementValue = serde_json::from_str("72").unwrap();
        assert_eq!(value.get(), 72.0);
        assert_eq!(serde_json::to_string(&value).unwrap(), "72.0");
    }

    #[test]
    fn serde_rejects_non_numbers_and_overflow() {
        for bad in ["\"72\"", "null", "true", "[1]", "1e999"] {
            let err = serde_json::from_str::<Finite>(bad).expect_err(bad);
            assert!(!err.to_string().is_empty());
        }
    }

    #[test]
    fn display_is_the_shortest_round_trip_form() {
        assert_eq!(Finite::new(36.6).unwrap().to_string(), "36.6");
        assert_eq!(Finite::new(100.0).unwrap().to_string(), "100");
        assert_eq!(MeasurementValue::new(0.1).unwrap().to_string(), "0.1");
    }
}
