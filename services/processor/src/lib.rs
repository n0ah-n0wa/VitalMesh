//! VitalMesh processor: the data processing service behind the API gateway.
//!
//! Module map:
//! - `config`: typed configuration loaded from the environment.
//! - `error`: the classified error type shared by every layer.
//! - `domain`: job identifiers and the job state machine.
//! - `concurrency`: the bounded admission limiter.
//! - `engine`: bounded, cancellable, time-limited job execution.
//! - `stats`: the statistical processing engine (descriptive statistics, rolling
//!   statistics, tumbling windows).
//! - `anomaly`: configurable, versioned anomaly detection over window results.
//! - `pipeline`: the end-to-end processing pipeline and its bounded executor.
//! - `state`: application state shared with request handlers.
//! - `telemetry`: structured logging.
//! - `requestid`: request and correlation identifiers.
//! - `transport`: the HTTP router, middleware and handlers.
//! - `lifecycle`: start-up, serving and graceful shutdown.

pub mod anomaly;
pub mod concurrency;
pub mod config;
pub mod domain;
pub mod engine;
pub mod error;
pub mod lifecycle;
pub mod pipeline;
pub mod requestid;
pub mod state;
pub mod stats;
pub mod telemetry;
pub mod transport;

/// Service name reported in logs and health responses.
pub const SERVICE_NAME: &str = "processor";

/// Build identifier, injected by the Makefile through `VITALMESH_VERSION`.
pub const VERSION: &str = match option_env!("VITALMESH_VERSION") {
    Some(version) => version,
    None => "dev",
};
