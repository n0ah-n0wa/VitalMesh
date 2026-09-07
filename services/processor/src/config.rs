//! Typed configuration loaded from environment variables. Every invalid value
//! is reported at once so that start-up fails with a complete list instead of
//! the first problem found.

use std::fmt;
use std::net::SocketAddr;
use std::num::NonZeroUsize;
use std::time::Duration;

use tracing::Level;

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
                    1 << 20,
                    "a positive number of bytes",
                    positive_usize,
                ),
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
            },
            shutdown_timeout: p.duration("SHUTDOWN_TIMEOUT", Duration::from_secs(30)),
        };

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
        assert_eq!(cfg.http.max_body_bytes, 1 << 20);
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
            ("LOG_LEVEL", "debug"),
            ("LOG_FORMAT", "text"),
            ("MAX_CONCURRENT_JOBS", "16"),
            ("MAX_BATCH_SIZE", "250"),
            ("PROCESSING_TIMEOUT", "5m"),
            ("SHUTDOWN_TIMEOUT", "1500ms"),
        ])
        .expect("valid overrides");

        assert_eq!(cfg.environment, Environment::Staging);
        assert_eq!(cfg.http.addr, "127.0.0.1:9000".parse().unwrap());
        assert_eq!(cfg.http.request_timeout, Duration::from_secs(2));
        assert_eq!(cfg.http.max_body_bytes, 4096);
        assert_eq!(cfg.log.level, Level::DEBUG);
        assert_eq!(cfg.log.format, LogFormat::Text);
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
