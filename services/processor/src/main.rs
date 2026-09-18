//! VitalMesh processor service binary.
//!
//! Usage:
//!
//!   processor                run the HTTP server
//!   processor healthcheck    probe this process's own /health and exit 0
//!                            when it answers; the container image has no
//!                            shell to probe it with
//!   processor version        print this build's release record as JSON
//!                            (docs/RELEASE.md)
//!
//! Exit codes: 0 on clean shutdown, 1 on a runtime failure, 2 on invalid
//! configuration.

use std::process::ExitCode;

use tokio::net::TcpListener;
use tokio_util::sync::CancellationToken;

use processor::config::Config;
use processor::telemetry::{self, ServiceInfo};
use processor::{SERVICE_NAME, VERSION, lifecycle};

#[tokio::main]
async fn main() -> ExitCode {
    // The two arguments this binary takes. The runtime image has no shell,
    // so the binary probes itself (see `processor::healthcheck`) and prints
    // its own release record.
    match std::env::args().nth(1).as_deref() {
        Some("healthcheck") => return ExitCode::from(processor::healthcheck::run().await),
        Some("version") => return print_version(),
        _ => {}
    }

    let config = match Config::from_env() {
        Ok(config) => config,
        Err(error) => {
            // The logger is configured from the config, so this is the one
            // message that cannot be structured.
            eprint!("{error}");
            return ExitCode::from(2);
        }
    };

    let info = ServiceInfo {
        name: SERVICE_NAME,
        version: VERSION,
        environment: config.environment.as_str(),
    };
    let traces = match telemetry::init(&config.log, &config.tracing, info) {
        Ok(provider) => provider,
        Err(error) => {
            eprintln!("cannot initialise logging: {error}");
            return ExitCode::from(1);
        }
    };
    if !config.tracing.enabled() {
        tracing::info!(
            "OTEL_EXPORTER_OTLP_ENDPOINT is not set: trace context is propagated but no spans are exported"
        );
    }

    let shutdown = CancellationToken::new();
    if let Err(error) = lifecycle::shutdown_on_signal(shutdown.clone()) {
        tracing::error!(error = %error, "cannot install signal handlers");
        return ExitCode::from(1);
    }

    let listener = match TcpListener::bind(config.http.addr).await {
        Ok(listener) => listener,
        Err(error) => {
            tracing::error!(error = %error, addr = %config.http.addr, "cannot bind listener");
            return ExitCode::from(1);
        }
    };

    let outcome = lifecycle::run(config, listener, shutdown).await;

    // Flush what was recorded before the process ends. A collector that has
    // gone away must not hold up exit, so the provider's own bounded
    // shutdown is what waits, not this.
    if let Some(Err(error)) = traces.map(|provider| provider.shutdown()) {
        tracing::warn!(error = %error, "recorded spans were not flushed");
    }

    match outcome {
        Ok(()) => ExitCode::SUCCESS,
        Err(error) => {
            tracing::error!(error = %error, "processor exited");
            ExitCode::from(1)
        }
    }
}

/// Prints the release record: what identifies this build. It reads no
/// configuration and opens no connection, so it answers inside a container
/// whose dependencies are unreachable, which is when it is most often
/// asked. Nothing it prints is a secret: versions and digests only.
fn print_version() -> ExitCode {
    let build = processor::build();
    match serde_json::to_string_pretty(&build) {
        Ok(json) => println!("{json}"),
        Err(error) => {
            eprintln!("writing the release record: {error}");
            return ExitCode::from(1);
        }
    }
    let missing = build.incomplete();
    if !missing.is_empty() {
        eprintln!("release record does not identify: {}", missing.join(", "));
        return ExitCode::from(1);
    }
    ExitCode::SUCCESS
}
