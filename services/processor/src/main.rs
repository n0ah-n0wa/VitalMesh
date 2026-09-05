//! VitalMesh processor service binary.

use std::net::SocketAddr;

use tokio::net::TcpListener;

const DEFAULT_ADDR: &str = "0.0.0.0:8081";

#[tokio::main]
async fn main() -> Result<(), Box<dyn std::error::Error>> {
    let addr: SocketAddr = std::env::var("HTTP_ADDR")
        .unwrap_or_else(|_| DEFAULT_ADDR.to_string())
        .parse()?;

    let shutdown = shutdown_signal()?;
    let listener = TcpListener::bind(addr).await?;
    println!(
        "{} {} listening on {}",
        processor::SERVICE_NAME,
        processor::VERSION,
        addr
    );

    axum::serve(listener, processor::app())
        .with_graceful_shutdown(shutdown)
        .await?;

    println!("{} stopped", processor::SERVICE_NAME);
    Ok(())
}

/// Installs the termination signal handlers and returns a future that resolves
/// on SIGINT (Ctrl+C) or, on Unix, SIGTERM. Handler installation is done up
/// front so that a failure aborts start-up instead of leaving a process that
/// cannot be stopped cleanly.
fn shutdown_signal() -> std::io::Result<impl Future<Output = ()>> {
    #[cfg(unix)]
    let mut sigterm = tokio::signal::unix::signal(tokio::signal::unix::SignalKind::terminate())?;

    Ok(async move {
        let ctrl_c = async {
            if let Err(err) = tokio::signal::ctrl_c().await {
                eprintln!("cannot listen for Ctrl+C: {err}");
                std::future::pending::<()>().await;
            }
        };

        #[cfg(unix)]
        let terminate = async move {
            sigterm.recv().await;
        };
        #[cfg(not(unix))]
        let terminate = std::future::pending::<()>();

        tokio::select! {
            _ = ctrl_c => {}
            _ = terminate => {}
        }
    })
}
