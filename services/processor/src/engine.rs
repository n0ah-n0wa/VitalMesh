//! Bounded job execution. The engine admits at most `MAX_CONCURRENT_JOBS`
//! jobs, bounds each by `PROCESSING_TIMEOUT`, supports per-job cancellation
//! and cancels everything on shutdown. The processing algorithms themselves
//! are supplied as the future passed to [`Engine::run`]; they are not part
//! of this module.

use std::collections::HashMap;
use std::sync::{Mutex, MutexGuard, PoisonError};
use std::time::{Duration, Instant};

use tokio::sync::Notify;
use tokio_util::sync::CancellationToken;

use crate::concurrency::Limiter;
use crate::config::Processing;
use crate::domain::JobId;
use crate::error::{Error, Kind, Result};

/// Runs jobs within the configured limits.
#[derive(Debug)]
pub struct Engine {
    limiter: Limiter,
    timeout: Duration,
    shutdown: CancellationToken,
    running: Mutex<HashMap<JobId, RunningJob>>,
    idle: Notify,
}

#[derive(Debug)]
struct RunningJob {
    cancel: CancellationToken,
    started_at: Instant,
}

/// A point-in-time view of one running job.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct RunningJobInfo {
    pub id: JobId,
    pub running_for: Duration,
}

impl Engine {
    /// `shutdown` is cancelled by the lifecycle when in-flight jobs must stop.
    pub fn new(processing: &Processing, shutdown: CancellationToken) -> Self {
        Self {
            limiter: Limiter::new(processing.max_concurrent_jobs),
            timeout: processing.timeout,
            shutdown,
            running: Mutex::new(HashMap::new()),
            idle: Notify::new(),
        }
    }

    /// Runs `work` as job `id`.
    ///
    /// Fails without running when the engine is shutting down, at capacity,
    /// or already running a job with the same id. Otherwise the outcome is
    /// the work's result, a timeout error, or a cancellation error.
    pub async fn run<T, F>(&self, id: JobId, work: F) -> Result<T>
    where
        F: Future<Output = Result<T>>,
    {
        if self.shutdown.is_cancelled() {
            return Err(Error::new(
                Kind::Unavailable,
                "PROCESSOR_SHUTTING_DOWN",
                "The processor is shutting down.",
            ));
        }
        let _permit = self.limiter.try_acquire()?;
        let registration = self.register(id)?;

        tokio::select! {
            outcome = tokio::time::timeout(self.timeout, work) => match outcome {
                Ok(result) => result,
                Err(_elapsed) => Err(Error::timeout()),
            },
            () = registration.cancel.cancelled() => Err(Error::cancelled()),
        }
    }

    /// Requests cancellation of a running job. Returns whether the job was
    /// running.
    pub fn cancel(&self, id: &JobId) -> bool {
        match self.lock().get(id) {
            Some(job) => {
                job.cancel.cancel();
                true
            }
            None => false,
        }
    }

    /// The per-job time bound.
    pub fn timeout(&self) -> Duration {
        self.timeout
    }

    pub fn capacity(&self) -> usize {
        self.limiter.capacity()
    }

    pub fn active_jobs(&self) -> usize {
        self.lock().len()
    }

    /// Currently running jobs, in no particular order.
    pub fn running_jobs(&self) -> Vec<RunningJobInfo> {
        let now = Instant::now();
        self.lock()
            .iter()
            .map(|(id, job)| RunningJobInfo {
                id: id.clone(),
                running_for: now.saturating_duration_since(job.started_at),
            })
            .collect()
    }

    /// Resolves once no job is running.
    pub async fn drained(&self) {
        loop {
            let notified = self.idle.notified();
            tokio::pin!(notified);
            notified.as_mut().enable();
            if self.active_jobs() == 0 {
                return;
            }
            notified.await;
        }
    }

    fn register(&self, id: JobId) -> Result<Registration<'_>> {
        let mut running = self.lock();
        if running.contains_key(&id) {
            return Err(Error::new(
                Kind::Conflict,
                "JOB_ALREADY_RUNNING",
                "A job with this id is already running.",
            ));
        }
        let cancel = self.shutdown.child_token();
        running.insert(
            id.clone(),
            RunningJob {
                cancel: cancel.clone(),
                started_at: Instant::now(),
            },
        );
        Ok(Registration {
            engine: self,
            id,
            cancel,
        })
    }

    fn lock(&self) -> MutexGuard<'_, HashMap<JobId, RunningJob>> {
        // The map is never left inconsistent by a panic, so a poisoned lock
        // is safe to keep using.
        self.running.lock().unwrap_or_else(PoisonError::into_inner)
    }
}

/// Removes the job from the running set when the run ends, including when the
/// run future is dropped before completion.
struct Registration<'a> {
    engine: &'a Engine,
    id: JobId,
    cancel: CancellationToken,
}

impl Drop for Registration<'_> {
    fn drop(&mut self) {
        let remaining = {
            let mut running = self.engine.lock();
            running.remove(&self.id);
            running.len()
        };
        if remaining == 0 {
            self.engine.idle.notify_waiters();
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::num::NonZeroUsize;
    use std::time::Duration;

    fn engine(max_jobs: usize, timeout: Duration) -> (Engine, CancellationToken) {
        let shutdown = CancellationToken::new();
        let processing = Processing {
            max_concurrent_jobs: NonZeroUsize::new(max_jobs).unwrap(),
            max_batch_size: NonZeroUsize::MIN,
            max_job_measurements: NonZeroUsize::MIN,
            max_measurement_age: Duration::from_secs(1),
            max_future_skew: Duration::from_secs(1),
            timeout,
        };
        (Engine::new(&processing, shutdown.clone()), shutdown)
    }

    fn job(name: &str) -> JobId {
        JobId::parse(name).unwrap()
    }

    /// Handlers on the multi-threaded runtime need the run future to be Send.
    #[test]
    fn run_future_is_send() {
        fn assert_send<T: Send>(_: &T) {}
        let (engine, _) = engine(1, Duration::from_secs(1));
        let fut = engine.run(job("send"), async { Ok(()) });
        assert_send(&fut);
    }

    #[tokio::test]
    async fn returns_the_work_result() {
        let (engine, _) = engine(1, Duration::from_secs(1));
        let out = engine.run(job("a"), async { Ok(41 + 1) }).await.unwrap();
        assert_eq!(out, 42);
        assert_eq!(engine.active_jobs(), 0);
    }

    #[tokio::test]
    async fn rejects_when_at_capacity() {
        let (engine, _) = engine(1, Duration::from_secs(5));
        let (started_tx, started_rx) = tokio::sync::oneshot::channel();
        let (release_tx, release_rx) = tokio::sync::oneshot::channel::<()>();

        let first = engine.run(job("a"), async move {
            let _ = started_tx.send(());
            let _ = release_rx.await;
            Ok(())
        });
        tokio::pin!(first);

        // Drive the first job until it has started.
        tokio::select! {
            _ = &mut first => panic!("first job finished early"),
            _ = started_rx => {}
        }
        assert_eq!(engine.active_jobs(), 1);
        assert_eq!(engine.running_jobs()[0].id, job("a"));

        let err = engine
            .run(job("b"), async { Ok(()) })
            .await
            .expect_err("must be rejected");
        assert_eq!(err.kind(), Kind::Overloaded);

        let _ = release_tx.send(());
        first.await.unwrap();
        assert_eq!(engine.active_jobs(), 0);
        engine
            .run(job("b"), async { Ok(()) })
            .await
            .expect("capacity is back");
    }

    #[tokio::test]
    async fn times_out_slow_work() {
        let (engine, _) = engine(1, Duration::from_millis(20));
        let err = engine
            .run(job("slow"), async {
                tokio::time::sleep(Duration::from_secs(10)).await;
                Ok(())
            })
            .await
            .expect_err("must time out");
        assert_eq!(err.kind(), Kind::Timeout);
        assert_eq!(engine.active_jobs(), 0);
    }

    #[tokio::test]
    async fn cancel_stops_a_running_job() {
        let (engine, _) = engine(1, Duration::from_secs(10));
        let run = engine.run(job("c"), std::future::pending::<Result<()>>());
        tokio::pin!(run);

        tokio::select! {
            _ = &mut run => panic!("pending work completed"),
            _ = tokio::task::yield_now() => {}
        }
        assert!(engine.cancel(&job("c")), "job should be running");
        assert!(!engine.cancel(&job("nope")));

        let err = run.await.expect_err("cancelled");
        assert_eq!(err.kind(), Kind::Cancelled);
        assert_eq!(engine.active_jobs(), 0);
    }

    #[tokio::test]
    async fn shutdown_cancels_running_jobs_and_refuses_new_ones() {
        let (engine, shutdown) = engine(2, Duration::from_secs(10));
        let run = engine.run(job("d"), std::future::pending::<Result<()>>());
        tokio::pin!(run);
        tokio::select! {
            _ = &mut run => panic!("pending work completed"),
            _ = tokio::task::yield_now() => {}
        }

        shutdown.cancel();
        assert_eq!(run.await.expect_err("cancelled").kind(), Kind::Cancelled);

        let err = engine
            .run(job("e"), async { Ok(()) })
            .await
            .expect_err("refused");
        assert_eq!(err.kind(), Kind::Unavailable);
    }

    #[tokio::test]
    async fn duplicate_ids_conflict() {
        let (engine, _) = engine(2, Duration::from_secs(10));
        let run = engine.run(job("dup"), std::future::pending::<Result<()>>());
        tokio::pin!(run);
        tokio::select! {
            _ = &mut run => panic!("pending work completed"),
            _ = tokio::task::yield_now() => {}
        }

        let err = engine
            .run(job("dup"), async { Ok(()) })
            .await
            .expect_err("duplicate");
        assert_eq!(err.kind(), Kind::Conflict);
        assert_eq!(
            engine.active_jobs(),
            1,
            "the rejected duplicate must not register"
        );
    }

    #[tokio::test]
    async fn dropping_the_run_future_unregisters_the_job() {
        let (engine, _) = engine(1, Duration::from_secs(10));
        {
            let run = engine.run(job("f"), std::future::pending::<Result<()>>());
            tokio::pin!(run);
            tokio::select! {
                _ = &mut run => panic!("pending work completed"),
                _ = tokio::task::yield_now() => {}
            }
            assert_eq!(engine.active_jobs(), 1);
        }
        assert_eq!(engine.active_jobs(), 0);
        tokio::time::timeout(Duration::from_secs(1), engine.drained())
            .await
            .expect("drained resolves once the job is gone");
    }

    #[tokio::test]
    async fn drained_waits_for_running_jobs() {
        let (engine, _) = engine(1, Duration::from_secs(10));
        let (release_tx, release_rx) = tokio::sync::oneshot::channel::<()>();
        let run = engine.run(job("g"), async move {
            let _ = release_rx.await;
            Ok(())
        });
        tokio::pin!(run);
        tokio::select! {
            _ = &mut run => panic!("job finished early"),
            _ = tokio::task::yield_now() => {}
        }

        let drained = engine.drained();
        tokio::pin!(drained);
        assert!(
            tokio::time::timeout(Duration::from_millis(20), &mut drained)
                .await
                .is_err(),
            "drained must not resolve while a job runs"
        );

        let _ = release_tx.send(());
        run.await.unwrap();
        tokio::time::timeout(Duration::from_secs(1), drained)
            .await
            .expect("drained after completion");
    }
}
