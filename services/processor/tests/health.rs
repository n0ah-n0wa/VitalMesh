//! Integration test: the router serves the health endpoints over a real TCP socket.

use std::net::SocketAddr;

use tokio::io::{AsyncReadExt, AsyncWriteExt};
use tokio::net::{TcpListener, TcpStream};

async fn spawn_app() -> SocketAddr {
    let listener = TcpListener::bind("127.0.0.1:0")
        .await
        .expect("bind ephemeral port");
    let addr = listener.local_addr().expect("local addr");
    tokio::spawn(async move {
        axum::serve(listener, processor::app())
            .await
            .expect("server failed");
    });
    addr
}

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
    let status_line = head.lines().next().expect("status line").to_string();
    (status_line, body.to_string())
}

#[tokio::test]
async fn health_endpoints_return_ok_json() {
    let addr = spawn_app().await;

    for path in ["/health", "/ready"] {
        let (status, body) = get(addr, path).await;
        assert_eq!(status, "HTTP/1.1 200 OK", "{path}");

        let json: serde_json::Value = serde_json::from_str(&body).expect("json body");
        assert_eq!(json["status"], "ok", "{path}");
        assert_eq!(json["service"], processor::SERVICE_NAME, "{path}");
        assert_eq!(json["version"], processor::VERSION, "{path}");
    }
}

#[tokio::test]
async fn unknown_route_returns_404() {
    let addr = spawn_app().await;
    let (status, _) = get(addr, "/missing").await;
    assert_eq!(status, "HTTP/1.1 404 Not Found");
}
