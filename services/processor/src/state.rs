//! Application state shared with request handlers. One `Arc` is created by
//! the lifecycle and cloned into the router; nothing inside needs further
//! reference counting.

use std::sync::Arc;
use std::sync::atomic::{AtomicBool, Ordering};

use tokio_util::sync::CancellationToken;

use crate::config::Config;
use crate::engine::Engine;

/// Everything handlers need.
#[derive(Debug)]
pub struct AppState {
    pub config: Config,
    pub engine: Engine,
    pub readiness: Readiness,
}

/// Shared handle to the application state.
pub type SharedState = Arc<AppState>;

impl AppState {
    /// `jobs_cancel` is cancelled by the lifecycle when running jobs must stop.
    pub fn new(config: Config, jobs_cancel: CancellationToken) -> Self {
        Self {
            engine: Engine::new(&config.processing, jobs_cancel),
            readiness: Readiness::default(),
            config,
        }
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
