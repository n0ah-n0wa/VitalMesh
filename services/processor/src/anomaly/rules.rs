//! Rule configuration: the wire shape, validation and accessors.
//!
//! A rule set is JSON such as:
//!
//! ```json
//! {
//!   "rules": [
//!     {"name": "hr-bounds", "kind": "THRESHOLD", "measurement_types": ["HEART_RATE"],
//!      "tiers": {"warning": {"lower": 40, "upper": 180}, "critical": {"lower": 30, "upper": 220}}},
//!     {"name": "outliers", "kind": "Z_SCORE", "min_count": 5,
//!      "tiers": {"info": 2, "warning": 3, "critical": 4}},
//!     {"name": "drift", "kind": "ROLLING_DEVIATION", "window_size": 10, "min_count": 5,
//!      "windows": ["1h", "24h"], "tiers": {"warning": 3, "critical": 4.5}}
//!   ]
//! }
//! ```
//!
//! Unknown fields are rejected, tiers must be consistent (outer threshold
//! bounds contain inner ones; z-score and rolling thresholds increase with
//! severity and are positive), and every rule needs at least one tier.

use std::fmt;
use std::num::NonZeroUsize;

use serde::{Deserialize, Serialize};

use crate::domain::{Finite, MeasurementType, Severity, Window};

/// A rule set could not be built from its configuration.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct RuleError {
    pub rule: String,
    pub reason: String,
}

impl fmt::Display for RuleError {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        write!(f, "rule {:?}: {}", self.rule, self.reason)
    }
}

impl std::error::Error for RuleError {}

/// Per-severity configuration of one rule. At least one tier is set.
#[derive(Debug, Clone, PartialEq, Eq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct Tiers<T> {
    #[serde(skip_serializing_if = "Option::is_none")]
    pub info: Option<T>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub warning: Option<T>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub critical: Option<T>,
}

impl<T> Tiers<T> {
    /// Tiers from the most to the least severe, so the first match wins.
    pub fn descending(&self) -> impl Iterator<Item = (Severity, &T)> {
        [
            (Severity::Critical, self.critical.as_ref()),
            (Severity::Warning, self.warning.as_ref()),
            (Severity::Info, self.info.as_ref()),
        ]
        .into_iter()
        .filter_map(|(s, t)| t.map(|t| (s, t)))
    }

    fn is_empty(&self) -> bool {
        self.info.is_none() && self.warning.is_none() && self.critical.is_none()
    }
}

/// Inclusive acceptance interval of a threshold tier; a value strictly
/// below `lower` or strictly above `upper` crosses it. At least one side
/// is set.
#[derive(Debug, Clone, Copy, PartialEq, Eq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct Bounds {
    #[serde(skip_serializing_if = "Option::is_none")]
    pub lower: Option<Finite>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub upper: Option<Finite>,
}

impl Bounds {
    /// The bound `value` crosses, if any.
    pub fn crossed_by(&self, value: Finite) -> Option<Finite> {
        if let Some(lower) = self.lower
            && value < lower
        {
            return Some(lower);
        }
        if let Some(upper) = self.upper
            && value > upper
        {
            return Some(upper);
        }
        None
    }

    /// Whether every value accepted by `inner` is accepted by `self`.
    fn contains(&self, inner: &Bounds) -> bool {
        let lower_ok = match (self.lower, inner.lower) {
            (Some(outer), Some(inner)) => outer <= inner,
            (Some(_), None) => false,
            (None, _) => true,
        };
        let upper_ok = match (self.upper, inner.upper) {
            (Some(outer), Some(inner)) => outer >= inner,
            (Some(_), None) => false,
            (None, _) => true,
        };
        lower_ok && upper_ok
    }
}

#[derive(Debug, Clone, PartialEq, Eq)]
pub struct ThresholdRule {
    pub tiers: Tiers<Bounds>,
}

#[derive(Debug, Clone, PartialEq, Eq)]
pub struct ZScoreRule {
    /// Minimum number of values in the window; at least 2.
    pub min_count: NonZeroUsize,
    /// `|z|` must exceed the tier's value; positive and increasing.
    pub tiers: Tiers<Finite>,
}

#[derive(Debug, Clone, PartialEq, Eq)]
pub struct RollingDeviationRule {
    /// Number of preceding values the baseline is computed from; at least 2.
    pub window_size: NonZeroUsize,
    /// Minimum number of preceding values before a value is judged;
    /// between 2 and `window_size`.
    pub min_count: NonZeroUsize,
    /// `|value - mean|` must exceed `tier * std_dev`; positive, increasing.
    pub tiers: Tiers<Finite>,
}

#[derive(Debug, Clone, PartialEq, Eq)]
pub enum RuleKind {
    Threshold(ThresholdRule),
    ZScore(ZScoreRule),
    RollingDeviation(RollingDeviationRule),
}

/// One validated rule.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct Rule {
    name: String,
    measurement_types: Vec<MeasurementType>,
    windows: Vec<Window>,
    kind: RuleKind,
}

impl Rule {
    pub fn name(&self) -> &str {
        &self.name
    }

    pub fn kind(&self) -> &RuleKind {
        &self.kind
    }

    /// Whether the rule is scoped to this type and window. Empty scopes
    /// match everything.
    pub fn applies_to(&self, measurement_type: MeasurementType, window: Window) -> bool {
        (self.measurement_types.is_empty() || self.measurement_types.contains(&measurement_type))
            && (self.windows.is_empty() || self.windows.contains(&window))
    }
}

/// A validated, ordered list of rules. Evaluation order is declaration
/// order, which also breaks ties in the output.
#[derive(Debug, Clone, PartialEq, Eq, Deserialize)]
#[serde(try_from = "RawRuleSet")]
pub struct RuleSet {
    rules: Vec<Rule>,
}

impl RuleSet {
    pub fn new(rules: Vec<Rule>) -> Self {
        Self { rules }
    }

    /// Parses and validates a JSON rule set.
    pub fn from_json(json: &str) -> Result<Self, RuleError> {
        serde_json::from_str(json).map_err(|err| RuleError {
            rule: String::new(),
            reason: err.to_string(),
        })
    }

    /// A rule set with no rules: nothing is ever flagged.
    pub fn empty() -> Self {
        Self { rules: Vec::new() }
    }

    pub fn rules(&self) -> &[Rule] {
        &self.rules
    }

    /// Builds a validated rule.
    pub fn rule(
        name: &str,
        measurement_types: Vec<MeasurementType>,
        windows: Vec<Window>,
        kind: RuleKind,
    ) -> Result<Rule, RuleError> {
        let fail = |reason: String| RuleError {
            rule: name.to_owned(),
            reason,
        };
        if name.trim().is_empty() {
            return Err(fail("name is required".into()));
        }
        match &kind {
            RuleKind::Threshold(r) => validate_threshold(&r.tiers).map_err(fail)?,
            RuleKind::ZScore(r) => {
                if r.min_count.get() < 2 {
                    return Err(fail("min_count must be at least 2".into()));
                }
                validate_increasing(&r.tiers).map_err(fail)?;
            }
            RuleKind::RollingDeviation(r) => {
                if r.window_size.get() < 2 {
                    return Err(fail("window_size must be at least 2".into()));
                }
                if r.min_count.get() < 2 || r.min_count > r.window_size {
                    return Err(fail("min_count must be between 2 and window_size".into()));
                }
                validate_increasing(&r.tiers).map_err(fail)?;
            }
        }
        let mut measurement_types = measurement_types;
        measurement_types.sort();
        measurement_types.dedup();
        let mut windows = windows;
        windows.sort();
        windows.dedup();
        Ok(Rule {
            name: name.to_owned(),
            measurement_types,
            windows,
            kind,
        })
    }
}

fn validate_threshold(tiers: &Tiers<Bounds>) -> Result<(), String> {
    if tiers.is_empty() {
        return Err("at least one tier is required".into());
    }
    let mut previous: Option<(Severity, &Bounds)> = None;
    // Walk from the least to the most severe: each must contain the last.
    let ascending: Vec<(Severity, &Bounds)> = {
        let mut v: Vec<_> = tiers.descending().collect();
        v.reverse();
        v
    };
    for (severity, bounds) in ascending {
        if bounds.lower.is_none() && bounds.upper.is_none() {
            return Err(format!("{severity:?} tier needs a lower or an upper bound"));
        }
        if let (Some(lo), Some(hi)) = (bounds.lower, bounds.upper)
            && lo > hi
        {
            return Err(format!("{severity:?} tier has lower above upper"));
        }
        if let Some((inner_severity, inner)) = previous
            && !bounds.contains(inner)
        {
            return Err(format!(
                "{severity:?} tier must contain the {inner_severity:?} tier"
            ));
        }
        previous = Some((severity, bounds));
    }
    Ok(())
}

fn validate_increasing(tiers: &Tiers<Finite>) -> Result<(), String> {
    if tiers.is_empty() {
        return Err("at least one tier is required".into());
    }
    let mut previous: Option<(Severity, Finite)> = None;
    let mut ascending: Vec<(Severity, Finite)> = tiers.descending().map(|(s, t)| (s, *t)).collect();
    ascending.reverse();
    for (severity, threshold) in ascending {
        if threshold <= Finite::ZERO {
            return Err(format!("{severity:?} threshold must be positive"));
        }
        if let Some((inner_severity, inner)) = previous
            && threshold <= inner
        {
            return Err(format!(
                "{severity:?} threshold must exceed the {inner_severity:?} threshold"
            ));
        }
        previous = Some((severity, threshold));
    }
    Ok(())
}

#[derive(Debug, Deserialize)]
#[serde(deny_unknown_fields)]
struct RawRuleSet {
    rules: Vec<RawRule>,
}

#[derive(Debug, Deserialize)]
#[serde(deny_unknown_fields)]
struct RawRule {
    name: String,
    kind: RawKind,
    #[serde(default)]
    measurement_types: Vec<MeasurementType>,
    #[serde(default)]
    windows: Vec<Window>,
    tiers: serde_json::Value,
    #[serde(default)]
    min_count: Option<NonZeroUsize>,
    #[serde(default)]
    window_size: Option<NonZeroUsize>,
}

#[derive(Debug, Clone, Copy, Deserialize)]
#[serde(rename_all = "SCREAMING_SNAKE_CASE")]
enum RawKind {
    Threshold,
    ZScore,
    RollingDeviation,
}

impl TryFrom<RawRuleSet> for RuleSet {
    type Error = RuleError;

    fn try_from(raw: RawRuleSet) -> Result<Self, RuleError> {
        let mut rules = Vec::with_capacity(raw.rules.len());
        for r in raw.rules {
            let fail = |reason: String| RuleError {
                rule: r.name.clone(),
                reason,
            };
            let kind = match r.kind {
                RawKind::Threshold => {
                    if r.min_count.is_some() || r.window_size.is_some() {
                        return Err(fail(
                            "threshold rules take neither min_count nor window_size".into(),
                        ));
                    }
                    RuleKind::Threshold(ThresholdRule {
                        tiers: parse_tiers(&r.name, r.tiers)?,
                    })
                }
                RawKind::ZScore => {
                    if r.window_size.is_some() {
                        return Err(fail("z-score rules take no window_size".into()));
                    }
                    RuleKind::ZScore(ZScoreRule {
                        min_count: r.min_count.unwrap_or(DEFAULT_MIN_COUNT),
                        tiers: parse_tiers(&r.name, r.tiers)?,
                    })
                }
                RawKind::RollingDeviation => {
                    let window_size = r
                        .window_size
                        .ok_or_else(|| fail("rolling deviation rules need window_size".into()))?;
                    RuleKind::RollingDeviation(RollingDeviationRule {
                        window_size,
                        min_count: r.min_count.unwrap_or(window_size.min(DEFAULT_MIN_COUNT)),
                        tiers: parse_tiers(&r.name, r.tiers)?,
                    })
                }
            };
            rules.push(Self::rule(&r.name, r.measurement_types, r.windows, kind)?);
        }
        Ok(Self { rules })
    }
}

/// Parses the tiers of one rule into the shape its kind expects.
fn parse_tiers<T: serde::de::DeserializeOwned>(
    rule: &str,
    value: serde_json::Value,
) -> Result<Tiers<T>, RuleError> {
    serde_json::from_value(value).map_err(|e| RuleError {
        rule: rule.to_owned(),
        reason: format!("tiers: {e}"),
    })
}

/// Default minimum sample size for statistical rules when none is given.
const DEFAULT_MIN_COUNT: NonZeroUsize = NonZeroUsize::new(5).unwrap();

#[cfg(test)]
mod tests {
    use super::*;

    fn fin(v: f64) -> Finite {
        Finite::new(v).unwrap()
    }

    #[test]
    fn parses_every_rule_kind_with_defaults_and_scopes() {
        let set = RuleSet::from_json(
            r#"{"rules": [
              {"name": "bounds", "kind": "THRESHOLD", "measurement_types": ["SPO2", "HEART_RATE", "SPO2"],
               "tiers": {"warning": {"lower": 40, "upper": 180}, "critical": {"lower": 30}}},
              {"name": "z", "kind": "Z_SCORE", "tiers": {"info": 2, "critical": 4}},
              {"name": "roll", "kind": "ROLLING_DEVIATION", "window_size": 3, "windows": ["24h", "1h"],
               "tiers": {"warning": 3}}
            ]}"#,
        )
        .unwrap();
        assert_eq!(set.rules().len(), 3);
        let bounds = &set.rules()[0];
        assert_eq!(bounds.name(), "bounds");
        assert!(bounds.applies_to(MeasurementType::Spo2, Window::OneMinute));
        assert!(bounds.applies_to(MeasurementType::HeartRate, Window::SevenDays));
        assert!(!bounds.applies_to(MeasurementType::BloodGlucose, Window::OneHour));
        assert_eq!(
            bounds.measurement_types,
            [MeasurementType::HeartRate, MeasurementType::Spo2]
        );
        match set.rules()[1].kind() {
            RuleKind::ZScore(z) => assert_eq!(z.min_count, DEFAULT_MIN_COUNT),
            other => panic!("{other:?}"),
        }
        let roll = &set.rules()[2];
        assert!(roll.applies_to(MeasurementType::Spo2, Window::OneHour));
        assert!(!roll.applies_to(MeasurementType::Spo2, Window::OneMinute));
        match roll.kind() {
            RuleKind::RollingDeviation(r) => {
                assert_eq!(r.window_size.get(), 3);
                assert_eq!(
                    r.min_count.get(),
                    3,
                    "min_count defaults to min(5, window_size)"
                );
            }
            other => panic!("{other:?}"),
        }
    }

    #[test]
    fn rejects_invalid_configurations() {
        let cases: &[(&str, &str)] = &[
            (
                r#"{"rules": [{"name": "", "kind": "THRESHOLD", "tiers": {"info": {"upper": 1}}}]}"#,
                "name is required",
            ),
            (
                r#"{"rules": [{"name": "t", "kind": "THRESHOLD", "tiers": {}}]}"#,
                "at least one tier",
            ),
            (
                r#"{"rules": [{"name": "t", "kind": "THRESHOLD", "tiers": {"info": {}}}]}"#,
                "lower or an upper",
            ),
            (
                r#"{"rules": [{"name": "t", "kind": "THRESHOLD", "tiers": {"info": {"lower": 5, "upper": 1}}}]}"#,
                "lower above upper",
            ),
            (
                r#"{"rules": [{"name": "t", "kind": "THRESHOLD", "tiers": {"info": {"lower": 1, "upper": 10}, "warning": {"lower": 2, "upper": 20}}}]}"#,
                "Warning tier must contain the Info tier",
            ),
            (
                r#"{"rules": [{"name": "t", "kind": "THRESHOLD", "tiers": {"warning": {"lower": 1}, "critical": {"upper": 20}}}]}"#,
                "Critical tier must contain the Warning tier",
            ),
            (
                r#"{"rules": [{"name": "t", "kind": "THRESHOLD", "min_count": 3, "tiers": {"info": {"upper": 1}}}]}"#,
                "neither min_count",
            ),
            (
                r#"{"rules": [{"name": "z", "kind": "Z_SCORE", "tiers": {"info": 0}}]}"#,
                "must be positive",
            ),
            (
                r#"{"rules": [{"name": "z", "kind": "Z_SCORE", "tiers": {"info": 3, "warning": 2}}]}"#,
                "Warning threshold must exceed the Info",
            ),
            (
                r#"{"rules": [{"name": "z", "kind": "Z_SCORE", "tiers": {"warning": 2, "critical": 2}}]}"#,
                "Critical threshold must exceed the Warning",
            ),
            (
                r#"{"rules": [{"name": "z", "kind": "Z_SCORE", "min_count": 1, "tiers": {"info": 2}}]}"#,
                "min_count must be at least 2",
            ),
            (
                r#"{"rules": [{"name": "z", "kind": "Z_SCORE", "window_size": 4, "tiers": {"info": 2}}]}"#,
                "no window_size",
            ),
            (
                r#"{"rules": [{"name": "r", "kind": "ROLLING_DEVIATION", "tiers": {"info": 2}}]}"#,
                "need window_size",
            ),
            (
                r#"{"rules": [{"name": "r", "kind": "ROLLING_DEVIATION", "window_size": 1, "tiers": {"info": 2}}]}"#,
                "window_size must be at least 2",
            ),
            (
                r#"{"rules": [{"name": "r", "kind": "ROLLING_DEVIATION", "window_size": 4, "min_count": 5, "tiers": {"info": 2}}]}"#,
                "between 2 and window_size",
            ),
            (
                r#"{"rules": [{"name": "r", "kind": "ROLLING_DEVIATION", "window_size": 0, "tiers": {"info": 2}}]}"#,
                "nonzero",
            ),
            (
                r#"{"rules": [{"name": "t", "kind": "THRESHOLD", "tiers": {"info": {"upper": 1}}, "extra": 1}]}"#,
                "unknown field",
            ),
            (
                r#"{"rules": [{"name": "t", "kind": "THRESHOLD", "tiers": {"info": {"upper": 1, "top": 2}}}]}"#,
                "unknown field",
            ),
            (
                r#"{"rules": [{"name": "t", "kind": "THRESHOLD", "tiers": {"severe": {"upper": 1}}}]}"#,
                "unknown field",
            ),
            (
                r#"{"rules": [{"name": "t", "kind": "BOGUS", "tiers": {}}]}"#,
                "unknown variant",
            ),
            (
                r#"{"rules": [{"name": "t", "kind": "THRESHOLD", "tiers": {"info": {"upper": "1e999"}}}]}"#,
                "tiers",
            ),
            (
                r#"{"rules": [{"name": "t", "kind": "THRESHOLD", "windows": ["2h"], "tiers": {"info": {"upper": 1}}}]}"#,
                "unknown variant",
            ),
        ];
        for (json, want) in cases {
            let err = RuleSet::from_json(json).expect_err(json);
            assert!(err.to_string().contains(want), "{json}: {err}");
        }
    }

    #[test]
    fn bounds_crossing_is_strict() {
        let b = Bounds {
            lower: Some(fin(40.0)),
            upper: Some(fin(180.0)),
        };
        assert_eq!(b.crossed_by(fin(40.0)), None);
        assert_eq!(b.crossed_by(fin(180.0)), None);
        assert_eq!(b.crossed_by(fin(39.999)), Some(fin(40.0)));
        assert_eq!(b.crossed_by(fin(180.001)), Some(fin(180.0)));
        let only_upper = Bounds {
            lower: None,
            upper: Some(fin(1.0)),
        };
        assert_eq!(only_upper.crossed_by(fin(-1e300)), None);
        assert_eq!(only_upper.crossed_by(fin(1.5)), Some(fin(1.0)));
    }

    #[test]
    fn tiers_iterate_from_critical_to_info() {
        let tiers = Tiers {
            info: Some(1),
            warning: None,
            critical: Some(3),
        };
        let order: Vec<_> = tiers.descending().collect();
        assert_eq!(order, [(Severity::Critical, &3), (Severity::Info, &1)]);
    }

    #[test]
    fn empty_rule_set_is_valid() {
        assert!(
            RuleSet::from_json(r#"{"rules": []}"#)
                .unwrap()
                .rules()
                .is_empty()
        );
        assert!(RuleSet::empty().rules().is_empty());
    }
}
