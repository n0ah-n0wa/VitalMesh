//! UTC timestamps.
//!
//! Input may carry any RFC 3339 offset (`2026-03-29T02:30:00+02:00`); it is
//! converted to UTC on parsing, so daylight-saving transitions and local
//! offsets never reach the engine. Output always uses `Z`. Precision is one
//! nanosecond. Supported years are 0000 to 9999 so that every value can be
//! formatted as RFC 3339.

use std::fmt;
use std::time::Duration;

use serde::{Deserialize, Deserializer, Serialize, Serializer};
use time::format_description::well_known::Rfc3339;
use time::{OffsetDateTime, UtcOffset};

use super::error::DomainError;

/// An instant in UTC.
#[derive(Debug, Clone, Copy, PartialEq, Eq, PartialOrd, Ord, Hash)]
pub struct Timestamp(OffsetDateTime);

impl Timestamp {
    /// Parses an RFC 3339 timestamp with any offset and normalises it to UTC.
    pub fn parse(raw: &str) -> Result<Self, DomainError> {
        let parsed =
            OffsetDateTime::parse(raw, &Rfc3339).map_err(|err| DomainError::InvalidTimestamp {
                input: raw.to_owned(),
                reason: err.to_string(),
            })?;
        Self::from_offset(parsed).ok_or_else(|| DomainError::InvalidTimestamp {
            input: raw.to_owned(),
            reason: "year must be between 0000 and 9999".to_owned(),
        })
    }

    /// The current instant in UTC, or `None` if the system clock reports a
    /// time outside the supported years.
    ///
    /// Processing never calls this. Every statistic and every anomaly is
    /// derived from a measurement's own `recorded_at` so that a result is
    /// reproducible; the wall clock is only for recording when this service
    /// did something, such as when it started a job.
    pub fn now() -> Option<Self> {
        Self::from_offset(OffsetDateTime::now_utc())
    }

    /// Builds a timestamp from whole seconds since the Unix epoch.
    pub fn from_unix_seconds(seconds: i64) -> Result<Self, DomainError> {
        OffsetDateTime::from_unix_timestamp(seconds)
            .ok()
            .and_then(Self::from_offset)
            .ok_or_else(|| DomainError::InvalidTimestamp {
                input: seconds.to_string(),
                reason: "outside the supported range".to_owned(),
            })
    }

    /// Builds a timestamp from nanoseconds since the Unix epoch.
    pub fn from_unix_nanos(nanos: i128) -> Result<Self, DomainError> {
        OffsetDateTime::from_unix_timestamp_nanos(nanos)
            .ok()
            .and_then(Self::from_offset)
            .ok_or_else(|| DomainError::InvalidTimestamp {
                input: nanos.to_string(),
                reason: "outside the supported range".to_owned(),
            })
    }

    fn from_offset(value: OffsetDateTime) -> Option<Self> {
        let utc = value.to_offset(UtcOffset::UTC);
        (0..=9999).contains(&utc.year()).then_some(Self(utc))
    }

    pub fn unix_seconds(self) -> i64 {
        self.0.unix_timestamp()
    }

    pub fn unix_nanos(self) -> i128 {
        self.0.unix_timestamp_nanos()
    }

    /// `self + duration`, or `None` if the result would leave the supported range.
    pub fn checked_add(self, duration: Duration) -> Option<Self> {
        let delta = time::Duration::try_from(duration).ok()?;
        self.0.checked_add(delta).and_then(Self::from_offset)
    }

    /// `self - duration`, or `None` if the result would leave the supported range.
    pub fn checked_sub(self, duration: Duration) -> Option<Self> {
        let delta = time::Duration::try_from(duration).ok()?;
        self.0.checked_sub(delta).and_then(Self::from_offset)
    }

    /// Time elapsed from `earlier` to `self`, or `None` if `earlier` is later.
    pub fn duration_since(self, earlier: Self) -> Option<Duration> {
        (self.0 - earlier.0).try_into().ok()
    }

    /// RFC 3339 in UTC, for example `2026-09-06T12:00:00Z` or
    /// `2026-09-06T12:00:00.5Z`.
    pub fn to_rfc3339(self) -> String {
        // Formatting only fails outside years 0000..=9999, which the
        // constructors exclude.
        self.0
            .format(&Rfc3339)
            .unwrap_or_else(|_| format!("{:?}", self.0))
    }
}

impl fmt::Display for Timestamp {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        f.write_str(&self.to_rfc3339())
    }
}

impl Serialize for Timestamp {
    fn serialize<S: Serializer>(&self, serializer: S) -> Result<S::Ok, S::Error> {
        serializer.serialize_str(&self.to_rfc3339())
    }
}

impl<'de> Deserialize<'de> for Timestamp {
    fn deserialize<D: Deserializer<'de>>(deserializer: D) -> Result<Self, D::Error> {
        let raw = String::deserialize(deserializer)?;
        Self::parse(&raw).map_err(serde::de::Error::custom)
    }
}

/// Acceptance window for client-supplied timestamps, checked against a
/// server clock value supplied by the caller so that the check is
/// deterministic.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub struct TimestampBounds {
    /// How far past `now` a timestamp may lie.
    pub max_future_skew: Duration,
    /// How far before `now` a timestamp may lie.
    pub max_age: Duration,
}

impl TimestampBounds {
    /// Rejects timestamps later than `now + max_future_skew` or earlier
    /// than `now - max_age`. Both limits are inclusive.
    pub fn check(&self, timestamp: Timestamp, now: Timestamp) -> Result<(), DomainError> {
        if let Some(latest) = now.checked_add(self.max_future_skew)
            && timestamp > latest
        {
            return Err(DomainError::TimestampTooFarInFuture {
                timestamp: timestamp.to_string(),
                limit: latest.to_string(),
            });
        }
        if let Some(earliest) = now.checked_sub(self.max_age)
            && timestamp < earliest
        {
            return Err(DomainError::TimestampTooOld {
                timestamp: timestamp.to_string(),
                limit: earliest.to_string(),
            });
        }
        Ok(())
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    fn ts(raw: &str) -> Timestamp {
        Timestamp::parse(raw).unwrap_or_else(|e| panic!("{raw}: {e}"))
    }

    #[test]
    fn now_is_a_usable_instant() {
        let now = Timestamp::now().expect("the system clock is within 0000-9999");
        // A round trip through the wire format must not lose the value.
        assert_eq!(Timestamp::parse(&now.to_rfc3339()).unwrap(), now);
        assert!(now > ts("2020-01-01T00:00:00Z"));
    }

    #[test]
    fn parses_offsets_and_normalises_to_utc() {
        // The same instant written three ways, including across a DST
        // boundary in Europe (2026-03-29 02:00 CET -> 03:00 CEST).
        let z = ts("2026-03-29T01:30:00Z");
        let cet = ts("2026-03-29T02:30:00+01:00");
        let cest = ts("2026-03-29T03:30:00+02:00");
        assert_eq!(z, cet);
        assert_eq!(z, cest);
        assert_eq!(cest.to_rfc3339(), "2026-03-29T01:30:00Z");
    }

    #[test]
    fn keeps_sub_second_precision_and_formats_deterministically() {
        assert_eq!(
            ts("2026-09-06T12:00:00.5Z").to_rfc3339(),
            "2026-09-06T12:00:00.5Z"
        );
        assert_eq!(
            ts("2026-09-06T12:00:00.123456789+00:00").to_rfc3339(),
            "2026-09-06T12:00:00.123456789Z"
        );
        assert_eq!(
            ts("2026-09-06T12:00:00.000Z").to_rfc3339(),
            "2026-09-06T12:00:00Z"
        );
        assert_eq!(
            ts("2026-09-06t12:00:00z").to_rfc3339(),
            "2026-09-06T12:00:00Z"
        );
    }

    #[test]
    fn leap_day_and_leap_second_handling() {
        assert_eq!(ts("2028-02-29T00:00:00Z").unix_seconds(), 1835395200);
        let err = Timestamp::parse("2027-02-29T00:00:00Z").unwrap_err();
        assert_eq!(err.code(), "INVALID_TIMESTAMP");
        // RFC 3339 allows :60; the crate folds it onto the next second.
        let leap = Timestamp::parse("2016-12-31T23:59:60Z");
        assert!(leap.is_ok() || matches!(leap, Err(DomainError::InvalidTimestamp { .. })));
    }

    #[test]
    fn rejects_impossible_and_malformed_timestamps() {
        for bad in [
            "",
            "2026-09-06",
            "2026-09-06T12:00:00",
            "2026-13-01T00:00:00Z",
            "2026-09-31T00:00:00Z",
            "2026-09-06T24:00:00Z",
            "2026-09-06T12:60:00Z",
            "1757160000",
            "yesterday",
            "2026-09-06T12:00:00+25:00",
        ] {
            let err = Timestamp::parse(bad).expect_err(bad);
            assert!(
                matches!(err, DomainError::InvalidTimestamp { .. }),
                "{bad}: {err}"
            );
        }
    }

    #[test]
    fn epoch_conversions_round_trip_and_reject_out_of_range() {
        let t = Timestamp::from_unix_seconds(1_788_696_000).unwrap();
        assert_eq!(t.to_rfc3339(), "2026-09-06T12:00:00Z");
        assert_eq!(Timestamp::from_unix_nanos(t.unix_nanos()).unwrap(), t);
        assert_eq!(
            Timestamp::from_unix_seconds(0).unwrap().to_rfc3339(),
            "1970-01-01T00:00:00Z"
        );

        assert!(
            Timestamp::from_unix_seconds(-62_167_219_201).is_err(),
            "year -1"
        );
        assert!(
            Timestamp::from_unix_seconds(253_402_300_800).is_err(),
            "year 10000"
        );
        assert!(Timestamp::from_unix_seconds(i64::MAX).is_err());
    }

    #[test]
    fn ordering_and_arithmetic() {
        let a = ts("2026-09-06T12:00:00Z");
        let b = ts("2026-09-06T12:00:01Z");
        assert!(a < b);
        assert_eq!(a.checked_add(Duration::from_secs(1)), Some(b));
        assert_eq!(b.checked_sub(Duration::from_secs(1)), Some(a));
        assert_eq!(b.duration_since(a), Some(Duration::from_secs(1)));
        assert_eq!(a.duration_since(b), None);
        assert_eq!(
            ts("9999-12-31T23:59:59Z").checked_add(Duration::from_secs(1)),
            None
        );
        assert_eq!(
            ts("0000-01-01T00:00:00Z").checked_sub(Duration::from_nanos(1)),
            None
        );
    }

    #[test]
    fn serde_round_trip_and_rejection() {
        let t = ts("2026-09-06T12:00:00.25Z");
        let json = serde_json::to_string(&t).unwrap();
        assert_eq!(json, "\"2026-09-06T12:00:00.25Z\"");
        assert_eq!(serde_json::from_str::<Timestamp>(&json).unwrap(), t);

        let shifted: Timestamp = serde_json::from_str("\"2026-09-06T14:00:00.25+02:00\"").unwrap();
        assert_eq!(shifted, t);

        for bad in ["\"2026-09-06\"", "1757160000", "null", "\"\""] {
            let err = serde_json::from_str::<Timestamp>(bad).expect_err(bad);
            assert!(!err.to_string().is_empty());
        }
    }

    #[test]
    fn bounds_reject_future_and_stale_timestamps_inclusively() {
        let now = ts("2026-09-06T12:00:00Z");
        let bounds = TimestampBounds {
            max_future_skew: Duration::from_secs(60),
            max_age: Duration::from_secs(3600),
        };

        bounds.check(now, now).unwrap();
        bounds.check(ts("2026-09-06T12:01:00Z"), now).unwrap();
        bounds.check(ts("2026-09-06T11:00:00Z"), now).unwrap();

        let future = bounds
            .check(ts("2026-09-06T12:01:00.000000001Z"), now)
            .unwrap_err();
        assert_eq!(future.code(), "TIMESTAMP_IN_FUTURE");
        assert!(future.to_string().contains("2026-09-06T12:01:00Z"));

        let stale = bounds
            .check(ts("2026-09-06T10:59:59.999999999Z"), now)
            .unwrap_err();
        assert_eq!(stale.code(), "TIMESTAMP_TOO_OLD");
    }
}
