//! Lifecycle test: the service serves over a real socket, reports readiness,
//! and stops cleanly when shutdown is requested.

use std::net::SocketAddr;
use std::time::Duration;

use tokio::io::{AsyncReadExt, AsyncWriteExt};
use tokio::net::{TcpListener, TcpStream};
use tokio_util::sync::CancellationToken;

use processor::config::Config;
use processor::lifecycle;

/// Sends a raw HTTP/1.1 GET and returns (status line, body).
async fn get(addr: SocketAddr, path: &str) -> (String, String) {
    let mut stream = TcpStream::connect(addr).await.expect("connect");
    let request = format!("GET {path} HTTP/1.1\r\nHost: localhost\r\nConnection: close\r\n\r\n");
    stream
        .write_all(request.as_bytes())
        .await
        .expect("write request");

    let mut raw = Vec::new();
    stream.read_to_end(&mut raw).await.expect("read response");
    let text = String::from_utf8(raw).expect("utf-8 response");
    let (head, body) = text.split_once("\r\n\r\n").expect("header/body separator");
    (
        head.lines().next().expect("status line").to_string(),
        body.to_string(),
    )
}

#[tokio::test]
async fn serves_until_shutdown_then_stops_cleanly() {
    let config = Config::load(|key| match key {
        "ENVIRONMENT" => Some("test".into()),
        "SHUTDOWN_TIMEOUT" => Some("1s".into()),
        _ => None,
    })
    .expect("valid config");
    let listener = TcpListener::bind("127.0.0.1:0").await.expect("bind");
    let addr = listener.local_addr().expect("addr");
    let shutdown = CancellationToken::new();

    let server = tokio::spawn(lifecycle::run(config, listener, shutdown.clone()));

    let (status, body) = get(addr, "/health").await;
    assert_eq!(status, "HTTP/1.1 200 OK");
    let health: serde_json::Value = serde_json::from_str(&body).expect("json");
    assert_eq!(health["status"], "ok");
    assert_eq!(health["service"], "processor");

    let (status, body) = get(addr, "/ready").await;
    assert_eq!(status, "HTTP/1.1 200 OK", "ready once listening: {body}");

    let (status, _) = get(addr, "/internal/v1/missing").await;
    assert_eq!(status, "HTTP/1.1 404 Not Found");

    shutdown.cancel();
    let outcome = tokio::time::timeout(Duration::from_secs(5), server)
        .await
        .expect("run must return after shutdown")
        .expect("task must not panic");
    outcome.expect("clean shutdown");

    assert!(
        TcpStream::connect(addr).await.is_err(),
        "listener must be closed after shutdown"
    );
}
