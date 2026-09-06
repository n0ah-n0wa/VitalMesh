//! Start-up, serving and graceful shutdown.
//!
//! On shutdown the service, in order: reports not ready, stops accepting
//! connections and finishes in-flight requests, waits up to
//! `SHUTDOWN_TIMEOUT` for running jobs, cancels any that remain, and exits.

use std::sync::Arc;

use tokio::net::TcpListener;
use tokio_util::sync::CancellationToken;

use crate::config::Config;
use crate::error::{Error, Result};
use crate::state::AppState;
use crate::transport;

/// Serves the internal API on `listener` until `shutdown` is cancelled, then
/// drains as described in the module documentation.
pub async fn run(config: Config, listener: TcpListener, shutdown: CancellationToken) -> Result<()> {
    let jobs_cancel = CancellationToken::new();
    let shutdown_timeout = config.shutdown_timeout;
    let state = Arc::new(AppState::new(config, jobs_cancel.clone()));
    let addr = listener.local_addr().map_err(Error::internal)?;

    let drain_watch = tokio::spawn({
        let state = state.clone();
        let shutdown = shutdown.clone();
        async move {
            shutdown.cancelled().await;
            state.readiness.set_ready(false);
            tracing::info!("shutdown requested; draining");
        }
    });

    state.readiness.set_ready(true);
    tracing::info!(addr = %addr, "listening");

    let serve_result = axum::serve(listener, transport::router(state.clone()))
        .with_graceful_shutdown({
            let shutdown = shutdown.clone();
            async move { shutdown.cancelled().await }
        })
        .await;
    drain_watch.abort();
    serve_result.map_err(Error::internal)?;

    if tokio::time::timeout(shutdown_timeout, state.engine.drained())
        .await
        .is_err()
    {
        tracing::warn!(
            active_jobs = state.engine.active_jobs(),
            "shutdown timeout elapsed; cancelling running jobs"
        );
        jobs_cancel.cancel();
        state.engine.drained().await;
    }

    tracing::info!("stopped");
    Ok(())
}

/// Spawns a task that cancels `shutdown` on SIGINT (Ctrl+C) or, on Unix,
/// SIGTERM. Handlers are installed before returning so that a failure to
/// install them aborts start-up.
pub fn shutdown_on_signal(
    shutdown: CancellationToken,
) -> std::io::Result<tokio::task::JoinHandle<()>> {
    #[cfg(unix)]
    let mut terminate = tokio::signal::unix::signal(tokio::signal::unix::SignalKind::terminate())?;

    Ok(tokio::spawn(async move {
        let ctrl_c = async {
            if let Err(error) = tokio::signal::ctrl_c().await {
                tracing::error!(error = %error, "cannot listen for Ctrl+C");
                std::future::pending::<()>().await;
            }
        };
        #[cfg(unix)]
        let terminate = async move {
            terminate.recv().await;
        };
        #[cfg(not(unix))]
        let terminate = std::future::pending::<()>();

        tokio::select! {
            () = ctrl_c => {}
            () = terminate => {}
        }
        tracing::info!("termination signal received");
        shutdown.cancel();
    }))
}
