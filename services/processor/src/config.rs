//! Typed configuration loaded from environment variables. Every invalid value
//! is reported at once so that start-up fails with a complete list instead of
//! the first problem found.

use std::fmt;
use std::net::SocketAddr;
use std::num::NonZeroUsize;
use std::time::Duration;

use tracing::Level;

use crate::anomaly::RuleSet;

/// Deployment environment.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum Environment {
    Local,
    Test,
    Staging,
    Production,
}

impl Environment {
    fn parse(raw: &str) -> Option<Self> {
        match raw {
            "local" => Some(Self::Local),
            "test" => Some(Self::Test),
            "staging" => Some(Self::Staging),
            "production" => Some(Self::Production),
            _ => None,
        }
    }

    /// Whether this environment is a real deployment. Some settings are
    /// optional for a developer and required in a deployment.
    pub fn is_deployed(self) -> bool {
        matches!(self, Self::Staging | Self::Production)
    }

    /// The canonical name used in logs.
    pub fn as_str(self) -> &'static str {
        match self {
            Self::Local => "local",
            Self::Test => "test",
            Self::Staging => "staging",
            Self::Production => "production",
        }
    }
}

/// A configured secret. It never appears in a log line, a debug rendering
/// or an error message: the only way to read it is [`Secret::reveal`], and
/// the only comparison offered runs in constant time so that a wrong token
/// cannot be discovered one byte at a time.
#[derive(Clone)]
pub struct Secret(String);

impl Secret {
    pub fn new(value: impl Into<String>) -> Self {
        Self(value.into())
    }

    /// The secret itself. Every call site is a place a secret could leak, so
    /// keep them few and obvious.
    pub fn reveal(&self) -> &str {
        &self.0
    }

    /// Whether `candidate` equals this secret, in time that does not depend
    /// on how many leading bytes match. The length is not secret: a
    /// mismatched length returns early, which is what every practical
    /// constant-time comparison does.
    pub fn matches(&self, candidate: &str) -> bool {
        let expected = self.0.as_bytes();
        let actual = candidate.as_bytes();
        if expected.len() != actual.len() {
            return false;
        }
        let mut difference = 0u8;
        for (a, b) in expected.iter().zip(actual) {
            difference |= a ^ b;
        }
        difference == 0
    }
}

impl fmt::Debug for Secret {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        f.write_str("[redacted]")
    }
}

impl fmt::Display for Secret {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        f.write_str("[redacted]")
    }
}

/// Log encoding.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum LogFormat {
    Json,
    Text,
}

impl LogFormat {
    fn parse(raw: &str) -> Option<Self> {
        match raw {
            "json" => Some(Self::Json),
            "text" => Some(Self::Text),
            _ => None,
        }
    }
}

/// Complete, validated service configuration.
#[derive(Debug, Clone)]
pub struct Config {
    pub environment: Environment,
    pub http: Http,
    pub log: Log,
    pub tracing: Tracing,
    pub processing: Processing,
    /// How long in-flight jobs may run after shutdown is requested before
    /// they are cancelled.
    pub shutdown_timeout: Duration,
}

/// Internal HTTP listener settings.
#[derive(Debug, Clone)]
pub struct Http {
    pub addr: SocketAddr,
    /// Bound on handling one request; longer requests receive a timeout error.
    pub request_timeout: Duration,
    /// Largest accepted request body.
    pub max_body_bytes: usize,
    /// The shared secret the gateway presents as `Authorization: Bearer`.
    /// Required in staging and production; when it is unset in a local or
    /// test environment the internal endpoints are served without
    /// authentication, which start-up says out loud.
    pub internal_token: Option<Secret>,
}

/// Distributed tracing (SPECIFICATIONS.md section 40). With no endpoint
/// the service still reads and honours incoming W3C trace context; it
/// simply exports nothing of its own.
#[derive(Debug, Clone)]
pub struct Tracing {
    /// OTLP/HTTP collector. Empty means no exporter.
    pub endpoint: String,
    /// Bounds one export attempt.
    pub timeout: std::time::Duration,
}

impl Tracing {
    /// Whether spans are exported.
    pub fn enabled(&self) -> bool {
        !self.endpoint.is_empty()
    }
}

/// Logging settings.
#[derive(Debug, Clone)]
pub struct Log {
    pub level: Level,
    pub format: LogFormat,
}

/// Processing limits required by the specification.
#[derive(Debug, Clone)]
pub struct Processing {
    pub max_concurrent_jobs: NonZeroUsize,
    pub max_batch_size: NonZeroUsize,
    /// Largest number of readings one job may carry (SPECIFICATIONS.md
    /// sections 17 and 89); jobs above it are refused before parsing.
    pub max_job_measurements: NonZeroUsize,
    /// Oldest `recorded_at` accepted, relative to the job's request time.
    pub max_measurement_age: Duration,
    /// Furthest `recorded_at` ahead of the job's request time accepted.
    pub max_future_skew: Duration,
    pub timeout: Duration,
    /// How long a finished job stays observable through the jobs endpoint.
    pub job_retention: Duration,
    /// The anomaly rules every job is evaluated against. An empty set flags
    /// nothing, which is the default: rules are a deployment decision.
    pub rules: RuleSet,
}

/// The list of configuration problems found by [`Config::load`].
#[derive(Debug)]
pub struct ConfigError {
    problems: Vec<String>,
}

impl ConfigError {
    /// One entry per invalid variable, in declaration order.
    pub fn problems(&self) -> &[String] {
        &self.problems
    }
}

impl fmt::Display for ConfigError {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        writeln!(f, "invalid configuration:")?;
        for problem in &self.problems {
            writeln!(f, "  {problem}")?;
        }
        Ok(())
    }
}

impl std::error::Error for ConfigError {}

impl Config {
    /// Loads configuration from the process environment.
    pub fn from_env() -> Result<Self, ConfigError> {
        Self::load(|key| std::env::var(key).ok())
    }

    /// Loads configuration through `lookup`. Unset or empty values take their
    /// defaults.
    pub fn load(lookup: impl Fn(&str) -> Option<String>) -> Result<Self, ConfigError> {
        let mut p = Parser {
            lookup,
            problems: Vec::new(),
        };

        let config = Config {
            environment: p.value(
                "ENVIRONMENT",
                Environment::Local,
                "one of local, test, staging, production",
                Environment::parse,
            ),
            http: Http {
                addr: p.value(
                    "HTTP_ADDR",
                    default_addr(),
                    "a socket address such as 0.0.0.0:8081",
                    |s| s.parse().ok(),
                ),
                request_timeout: p.duration("HTTP_REQUEST_TIMEOUT", Duration::from_secs(30)),
                max_body_bytes: p.value(
                    "HTTP_MAX_BODY_BYTES",
                    DEFAULT_MAX_BODY_BYTES,
                    "a positive number of bytes",
                    positive_usize,
                ),
                internal_token: p.secret("INTERNAL_TOKEN", MIN_TOKEN_LEN),
            },
            log: Log {
                level: p.value(
                    "LOG_LEVEL",
                    Level::INFO,
                    "one of trace, debug, info, warn, error",
                    |s| s.parse().ok(),
                ),
                format: p.value(
                    "LOG_FORMAT",
                    LogFormat::Json,
                    "json or text",
                    LogFormat::parse,
                ),
            },
            tracing: Tracing {
                endpoint: p
                    .raw("OTEL_EXPORTER_OTLP_ENDPOINT")
                    .map(|v| v.trim().to_owned())
                    .unwrap_or_default(),
                timeout: p.duration("OTEL_EXPORTER_OTLP_TIMEOUT", Duration::from_secs(10)),
            },
            processing: Processing {
                max_concurrent_jobs: p.value(
                    "MAX_CONCURRENT_JOBS",
                    DEFAULT_MAX_CONCURRENT_JOBS,
                    "a positive integer",
                    |s| s.parse().ok(),
                ),
                max_batch_size: p.value(
                    "MAX_BATCH_SIZE",
                    DEFAULT_MAX_BATCH_SIZE,
                    "a positive integer",
                    |s| s.parse().ok(),
                ),
                max_job_measurements: p.value(
                    "MAX_JOB_MEASUREMENTS",
                    DEFAULT_MAX_JOB_MEASUREMENTS,
                    "a positive integer",
                    |s| s.parse().ok(),
                ),
                max_measurement_age: p
                    .duration("MAX_MEASUREMENT_AGE", Duration::from_secs(400 * 24 * 3600)),
                max_future_skew: p.duration("MAX_FUTURE_SKEW", Duration::from_secs(300)),
                timeout: p.duration("PROCESSING_TIMEOUT", Duration::from_secs(300)),
                job_retention: p.duration("JOB_RETENTION", Duration::from_secs(900)),
                rules: p.rules("ANOMALY_RULES"),
            },
            shutdown_timeout: p.duration("SHUTDOWN_TIMEOUT", Duration::from_secs(30)),
        };

        // Checks that depend on more than one value.
        // A body limit below what the job ceiling implies would refuse jobs
        // the health endpoint says this processor accepts, and the client
        // would see an opaque rejection rather than a clear limit.
        let needed = config
            .processing
            .max_job_measurements
            .get()
            .saturating_mul(BYTES_PER_READING);
        if config.http.max_body_bytes < needed {
            p.problems.push(format!(
                "HTTP_MAX_BODY_BYTES: {} is too small for MAX_JOB_MEASUREMENTS={}, which needs at \
                 least {needed} bytes; raise it or lower the job ceiling",
                config.http.max_body_bytes, config.processing.max_job_measurements
            ));
        }
        if config.environment.is_deployed() && config.http.internal_token.is_none() {
            p.problems.push(
                "INTERNAL_TOKEN: required in staging and production, where the internal API \
                 must not be reachable without a credential"
                    .to_owned(),
            );
        }

        if p.problems.is_empty() {
            Ok(config)
        } else {
            Err(ConfigError {
                problems: p.problems,
            })
        }
    }
}

const DEFAULT_MAX_CONCURRENT_JOBS: NonZeroUsize = NonZeroUsize::new(4).unwrap();
const DEFAULT_MAX_BATCH_SIZE: NonZeroUsize = NonZeroUsize::new(1000).unwrap();
const DEFAULT_MAX_JOB_MEASUREMENTS: NonZeroUsize = NonZeroUsize::new(100_000).unwrap();

/// Shortest accepted internal token. Short enough to be typed in a local
/// setup, long enough that a guessed one is not a realistic threat.
const MIN_TOKEN_LEN: usize = 16;

/// Bytes one reading occupies in a request body, rounded up: an identifier,
/// a type, a value, a unit and an RFC 3339 instant, with their field names
/// and punctuation.
pub const BYTES_PER_READING: usize = 128;

/// Largest accepted request body. The internal API carries a job's whole
/// measurement set in one body, so this must hold `MAX_JOB_MEASUREMENTS`
/// readings; a smaller limit would refuse jobs the processor advertises it
/// can take. [`Config::load`] checks that the two stay consistent whatever
/// they are configured to.
const DEFAULT_MAX_BODY_BYTES: usize = 16 << 20;

fn default_addr() -> SocketAddr {
    SocketAddr::from(([0, 0, 0, 0], 8081))
}

fn positive_usize(raw: &str) -> Option<usize> {
    raw.parse().ok().filter(|n| *n > 0)
}

/// Parses durations written as an integer with a unit: `250ms`, `30s`, `5m`, `1h`.
pub fn parse_duration(raw: &str) -> Option<Duration> {
    let raw = raw.trim();
    let split = raw.find(|c: char| !c.is_ascii_digit())?;
    let (number, unit) = raw.split_at(split);
    let n: u64 = number.parse().ok()?;
    let duration = match unit {
        "ms" => Duration::from_millis(n),
        "s" => Duration::from_secs(n),
        "m" => Duration::from_secs(n.checked_mul(60)?),
        "h" => Duration::from_secs(n.checked_mul(3600)?),
        _ => return None,
    };
    (duration > Duration::ZERO).then_some(duration)
}

struct Parser<F> {
    lookup: F,
    problems: Vec<String>,
}

impl<F: Fn(&str) -> Option<String>> Parser<F> {
    fn raw(&self, key: &str) -> Option<String> {
        (self.lookup)(key).filter(|v| !v.trim().is_empty())
    }

    fn value<T>(
        &mut self,
        key: &str,
        default: T,
        expected: &str,
        parse: impl FnOnce(&str) -> Option<T>,
    ) -> T {
        let Some(raw) = self.raw(key) else {
            return default;
        };
        match parse(raw.trim()) {
            Some(value) => value,
            None => {
                self.problems
                    .push(format!("{key}: expected {expected}, got {raw:?}"));
                default
            }
        }
    }

    fn duration(&mut self, key: &str, default: Duration) -> Duration {
        self.value(
            key,
            default,
            "a positive duration such as 500ms, 30s, 5m or 1h",
            parse_duration,
        )
    }

    /// A secret, absent when unset. The problem message names the key and
    /// the rule it broke and never echoes the value, unlike [`Self::value`].
    fn secret(&mut self, key: &str, min_len: usize) -> Option<Secret> {
        let raw = self.raw(key)?;
        let value = raw.trim();
        if value.len() < min_len {
            self.problems
                .push(format!("{key}: expected at least {min_len} characters"));
            return None;
        }
        Some(Secret::new(value))
    }

    /// A JSON rule set, empty when unset. The problem message carries the
    /// rule's own complaint but not the configured JSON, which can be long.
    fn rules(&mut self, key: &str) -> RuleSet {
        let Some(raw) = self.raw(key) else {
            return RuleSet::empty();
        };
        match RuleSet::from_json(raw.trim()) {
            Ok(rules) => rules,
            Err(error) => {
                self.problems.push(format!("{key}: {error}"));
                RuleSet::empty()
            }
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::collections::HashMap;

    fn load(values: &[(&str, &str)]) -> Result<Config, ConfigError> {
        let map: HashMap<String, String> = values
            .iter()
            .map(|(k, v)| (k.to_string(), v.to_string()))
            .collect();
        Config::load(|key| map.get(key).cloned())
    }

    #[test]
    fn defaults_apply_when_nothing_is_set() {
        let cfg = load(&[]).expect("defaults are valid");
        assert_eq!(cfg.environment, Environment::Local);
        assert_eq!(cfg.http.addr, "0.0.0.0:8081".parse().unwrap());
        assert_eq!(cfg.http.request_timeout, Duration::from_secs(30));
        assert_eq!(cfg.http.max_body_bytes, 16 << 20);
        assert_eq!(cfg.log.level, Level::INFO);
        assert_eq!(cfg.log.format, LogFormat::Json);
        assert_eq!(cfg.processing.max_concurrent_jobs.get(), 4);
        assert_eq!(cfg.processing.max_batch_size.get(), 1000);
        assert_eq!(cfg.processing.max_job_measurements.get(), 100_000);
        assert_eq!(
            cfg.processing.max_measurement_age,
            Duration::from_secs(400 * 24 * 3600)
        );
        assert_eq!(cfg.processing.max_future_skew, Duration::from_secs(300));
        assert_eq!(cfg.processing.timeout, Duration::from_secs(300));
        assert_eq!(cfg.shutdown_timeout, Duration::from_secs(30));
    }

    #[test]
    fn overrides_are_applied() {
        let cfg = load(&[
            ("ENVIRONMENT", "staging"),
            ("HTTP_ADDR", "127.0.0.1:9000"),
            ("HTTP_REQUEST_TIMEOUT", "2s"),
            ("HTTP_MAX_BODY_BYTES", "4096"),
            // A small body limit only makes sense with a small job ceiling.
            ("MAX_JOB_MEASUREMENTS", "32"),
            ("LOG_LEVEL", "debug"),
            ("LOG_FORMAT", "text"),
            ("MAX_CONCURRENT_JOBS", "16"),
            ("MAX_BATCH_SIZE", "250"),
            ("PROCESSING_TIMEOUT", "5m"),
            ("SHUTDOWN_TIMEOUT", "1500ms"),
            ("JOB_RETENTION", "2m"),
            ("INTERNAL_TOKEN", "example-token-not-a-secret"),
        ])
        .expect("valid overrides");

        assert_eq!(cfg.environment, Environment::Staging);
        assert_eq!(cfg.http.addr, "127.0.0.1:9000".parse().unwrap());
        assert_eq!(cfg.http.request_timeout, Duration::from_secs(2));
        assert_eq!(cfg.http.max_body_bytes, 4096);
        assert_eq!(cfg.processing.max_job_measurements.get(), 32);
        assert_eq!(cfg.log.level, Level::DEBUG);
        assert_eq!(cfg.log.format, LogFormat::Text);
        assert_eq!(cfg.processing.job_retention, Duration::from_secs(120));
        assert_eq!(
            cfg.http.internal_token.as_ref().map(Secret::reveal),
            Some("example-token-not-a-secret")
        );
        assert_eq!(cfg.processing.max_concurrent_jobs.get(), 16);
        assert_eq!(cfg.processing.max_batch_size.get(), 250);
        assert_eq!(cfg.processing.timeout, Duration::from_secs(300));
        assert_eq!(cfg.shutdown_timeout, Duration::from_millis(1500));
    }

    #[test]
    fn empty_values_count_as_unset() {
        let cfg = load(&[("HTTP_ADDR", ""), ("LOG_LEVEL", "   ")]).expect("empty is unset");
        assert_eq!(cfg.http.addr, default_addr());
        assert_eq!(cfg.log.level, Level::INFO);
    }

    #[test]
    fn every_invalid_value_is_reported() {
        let err = load(&[
            ("ENVIRONMENT", "prod"),
            ("HTTP_ADDR", "not-an-address"),
            ("HTTP_MAX_BODY_BYTES", "0"),
            ("LOG_LEVEL", "loud"),
            ("LOG_FORMAT", "xml"),
            ("MAX_CONCURRENT_JOBS", "0"),
            ("PROCESSING_TIMEOUT", "soon"),
        ])
        .expect_err("invalid values must fail");

        let keys: Vec<&str> = err
            .problems()
            .iter()
            .map(|p| p.split(':').next().unwrap())
            .collect();
        assert_eq!(
            keys,
            [
                "ENVIRONMENT",
                "HTTP_ADDR",
                "HTTP_MAX_BODY_BYTES",
                "LOG_LEVEL",
                "LOG_FORMAT",
                "MAX_CONCURRENT_JOBS",
                "PROCESSING_TIMEOUT",
            ]
        );
        let rendered = err.to_string();
        assert!(rendered.starts_with("invalid configuration:\n"));
        assert!(rendered.contains(
            "ENVIRONMENT: expected one of local, test, staging, production, got \"prod\""
        ));
    }

    /// A deployment must not serve the internal API without a credential.
    /// The two numbers the health endpoint advertises must not contradict
    /// each other: a job at the ceiling has to fit in a body.
    #[test]
    fn the_body_limit_must_hold_a_job_at_the_ceiling() {
        let cfg = load(&[]).expect("the defaults must agree");
        assert!(
            cfg.http.max_body_bytes
                >= cfg.processing.max_job_measurements.get() * BYTES_PER_READING,
            "the default body limit {} cannot hold {} readings",
            cfg.http.max_body_bytes,
            cfg.processing.max_job_measurements
        );

        let error = load(&[
            ("MAX_JOB_MEASUREMENTS", "100000"),
            ("HTTP_MAX_BODY_BYTES", "1048576"),
        ])
        .expect_err("a body limit too small for the job ceiling must not start");
        let problem = error.problems().join("\n");
        assert!(problem.contains("HTTP_MAX_BODY_BYTES"), "{problem}");
        assert!(problem.contains("MAX_JOB_MEASUREMENTS"), "{problem}");

        // Lowering the ceiling instead is a valid way to agree.
        load(&[
            ("MAX_JOB_MEASUREMENTS", "1000"),
            ("HTTP_MAX_BODY_BYTES", "1048576"),
        ])
        .expect("a smaller ceiling fits the smaller body");
    }

    #[test]
    fn a_deployment_requires_an_internal_token() {
        for environment in ["staging", "production"] {
            let error = load(&[("ENVIRONMENT", environment)])
                .expect_err("a deployment without a token must not start");
            assert!(
                error
                    .problems()
                    .iter()
                    .any(|p| p.starts_with("INTERNAL_TOKEN:")),
                "{environment}: {:?}",
                error.problems()
            );
        }
        for environment in ["local", "test"] {
            let cfg =
                load(&[("ENVIRONMENT", environment)]).expect("a developer may run without a token");
            assert!(cfg.http.internal_token.is_none());
        }
    }

    #[test]
    fn a_short_token_is_refused_and_never_echoed() {
        let error = load(&[("INTERNAL_TOKEN", "too-short")]).expect_err("a short token is refused");
        let problem = error.problems().join("\n");
        assert!(problem.contains("INTERNAL_TOKEN"), "{problem}");
        assert!(
            !problem.contains("too-short"),
            "the problem echoed the secret: {problem}"
        );
    }

    /// A secret must not be readable from a log line, a panic message or a
    /// debug rendering of the configuration.
    #[test]
    fn a_secret_never_renders_itself() {
        let secret = Secret::new("example-token-not-a-secret");
        assert_eq!(format!("{secret}"), "[redacted]");
        assert_eq!(format!("{secret:?}"), "[redacted]");

        let cfg = load(&[("INTERNAL_TOKEN", "example-token-not-a-secret")]).unwrap();
        let rendered = format!("{cfg:?}");
        assert!(
            !rendered.contains("example-token-not-a-secret"),
            "the configuration rendered its secret: {rendered}"
        );
        assert!(rendered.contains("[redacted]"));
        assert_eq!(secret.reveal(), "example-token-not-a-secret");
    }

    #[test]
    fn secret_comparison_accepts_only_the_exact_value() {
        let secret = Secret::new("example-token-value");
        assert!(secret.matches("example-token-value"));
        for wrong in [
            "example-token-valu",   // shorter
            "example-token-values", // longer
            "example-token-valuE",  // one byte different
            "",
            "Example-token-value", // differs in the first byte
        ] {
            assert!(!secret.matches(wrong), "{wrong:?} must not match");
        }
    }

    #[test]
    fn anomaly_rules_are_parsed_and_a_broken_set_is_reported_without_its_json() {
        let cfg = load(&[]).unwrap();
        assert!(
            cfg.processing.rules.rules().is_empty(),
            "the default flags nothing"
        );

        let cfg = load(&[(
            "ANOMALY_RULES",
            r#"{"rules": [{"name": "hr", "kind": "THRESHOLD",
                 "tiers": {"warning": {"upper": 180}}}]}"#,
        )])
        .expect("a valid rule set loads");
        assert_eq!(cfg.processing.rules.rules().len(), 1);
        assert_eq!(cfg.processing.rules.rules()[0].name(), "hr");

        let error = load(&[("ANOMALY_RULES", r#"{"rules": [{"name": "x"}]}"#)])
            .expect_err("an invalid rule set must stop start-up");
        let problem = error.problems().join("\n");
        assert!(problem.starts_with("ANOMALY_RULES:"), "{problem}");
        assert!(
            !problem.contains(r#"{"rules""#),
            "the problem echoed the configured JSON: {problem}"
        );
    }

    #[test]
    fn duration_parsing() {
        assert_eq!(parse_duration("250ms"), Some(Duration::from_millis(250)));
        assert_eq!(parse_duration("30s"), Some(Duration::from_secs(30)));
        assert_eq!(parse_duration("5m"), Some(Duration::from_secs(300)));
        assert_eq!(parse_duration("2h"), Some(Duration::from_secs(7200)));
        assert_eq!(parse_duration(" 1s "), Some(Duration::from_secs(1)));
        for bad in [
            "",
            "10",
            "s",
            "0s",
            "-1s",
            "1.5s",
            "1d",
            "1 s",
            "999999999999999999999h",
        ] {
            assert_eq!(parse_duration(bad), None, "{bad:?} should be rejected");
        }
    }
}
