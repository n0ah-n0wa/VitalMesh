//! Application state shared with request handlers. One `Arc` is created by
//! the lifecycle and cloned into the router; nothing inside needs further
//! reference counting.

use std::sync::Arc;
use std::sync::atomic::{AtomicBool, Ordering};

use tokio_util::sync::CancellationToken;

use crate::anomaly::Detector;
use crate::config::Config;
use crate::engine::Engine;
use crate::jobs::JobRegistry;
use crate::metrics::Metrics;
use crate::pipeline::{Limits, Pipeline, Processor};

/// Everything handlers need.
#[derive(Debug)]
pub struct AppState {
    pub config: Config,
    /// Runs pipeline cores under the engine's limits.
    pub processor: Processor,
    /// This instance's view of the jobs it is running or recently ran.
    pub jobs: JobRegistry,
    /// What the service publishes at `GET /metrics`.
    pub metrics: Metrics,
    pub readiness: Readiness,
}

/// Shared handle to the application state.
pub type SharedState = Arc<AppState>;

impl AppState {
    /// `jobs_cancel` is cancelled by the lifecycle when running jobs must stop.
    pub fn new(config: Config, jobs_cancel: CancellationToken) -> Self {
        let engine = Arc::new(Engine::new(&config.processing, jobs_cancel));
        let pipeline = Pipeline::new(
            Detector::new(config.processing.rules.clone()),
            Limits {
                max_job_measurements: config.processing.max_job_measurements,
                timestamp_bounds: crate::domain::TimestampBounds {
                    max_future_skew: config.processing.max_future_skew,
                    max_age: config.processing.max_measurement_age,
                },
            },
            crate::VERSION,
        );
        let processor = Processor::new(pipeline, engine);
        let metrics = Metrics::new();
        // The saturation gauges read the engine at scrape time, so they can
        // never drift from what admission actually sees.
        metrics.observe_engine(processor.engine_handle());
        Self {
            processor,
            jobs: JobRegistry::new(config.processing.job_retention),
            metrics,
            readiness: Readiness::default(),
            config,
        }
    }

    /// The execution engine, for callers that only need its limits or its
    /// drain signal.
    pub fn engine(&self) -> &Engine {
        self.processor.engine()
    }
}

/// Whether the service should receive traffic. It becomes ready once the
/// listener is bound and not ready as soon as shutdown begins, so a load
/// balancer stops routing to a draining instance. Dependency checks are
/// added together with the dependencies.
#[derive(Debug, Default)]
pub struct Readiness {
    ready: AtomicBool,
}

impl Readiness {
    pub fn set_ready(&self, ready: bool) {
        self.ready.store(ready, Ordering::Release);
    }

    pub fn is_ready(&self) -> bool {
        self.ready.load(Ordering::Acquire)
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    /// axum shares the state across worker threads.
    #[test]
    fn app_state_is_send_and_sync() {
        fn assert_send_sync<T: Send + Sync>() {}
        assert_send_sync::<AppState>();
        assert_send_sync::<SharedState>();
    }

    /// The pipeline must be built from the configured limits, not from
    /// defaults, or a deployment's bounds would be silently ignored.
    #[test]
    fn the_processor_is_built_from_the_configuration() {
        use std::num::NonZeroUsize;
        use std::time::Duration;

        let config = Config::load(|key| match key {
            "MAX_JOB_MEASUREMENTS" => Some("42".to_owned()),
            "MAX_FUTURE_SKEW" => Some("7s".to_owned()),
            "MAX_MEASUREMENT_AGE" => Some("9s".to_owned()),
            "MAX_CONCURRENT_JOBS" => Some("3".to_owned()),
            "JOB_RETENTION" => Some("11s".to_owned()),
            _ => None,
        })
        .expect("configuration must load");

        let state = AppState::new(config, CancellationToken::new());
        let limits = state.processor.pipeline().limits();
        assert_eq!(limits.max_job_measurements, NonZeroUsize::new(42).unwrap());
        assert_eq!(
            limits.timestamp_bounds.max_future_skew,
            Duration::from_secs(7)
        );
        assert_eq!(limits.timestamp_bounds.max_age, Duration::from_secs(9));
        assert_eq!(state.engine().capacity(), 3);
        assert_eq!(state.jobs.retention(), Duration::from_secs(11));
    }

    #[test]
    fn readiness_starts_false_and_toggles() {
        let readiness = Readiness::default();
        assert!(!readiness.is_ready());
        readiness.set_ready(true);
        assert!(readiness.is_ready());
        readiness.set_ready(false);
        assert!(!readiness.is_ready());
    }
}
