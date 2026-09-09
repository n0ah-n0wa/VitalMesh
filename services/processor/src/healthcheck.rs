//! The `healthcheck` argument: probe this process's own HTTP server and
//! report the answer as an exit code.
//!
//! It exists because the runtime image contains no shell, no curl and no
//! wget. A container built on a distroless base has nothing to run a health
//! check with except the binary already in it, and adding a shell so that
//! Docker can run `curl` would enlarge the attack surface far more than
//! this costs.
//!
//! The request is written to the socket by hand rather than through an HTTP
//! client. The service has no client dependency, and adding one so that it
//! can talk to itself would put a whole HTTP stack into the image for the
//! sake of four lines.
//!
//! It probes liveness (`/health`), not readiness. Readiness turns false the
//! moment shutdown begins, and a draining container is not one Docker
//! should restart; Kubernetes distinguishes the two with separate probes.

use std::time::Duration;

use tokio::io::{AsyncReadExt, AsyncWriteExt};
use tokio::net::TcpStream;

use crate::config::DEFAULT_HTTP_ADDR;

/// Bounds the probe. A probe slower than this has already said what the
/// orchestrator needed to know.
const TIMEOUT: Duration = Duration::from_secs(2);

/// Runs the probe. Returns 0 when the server answered 200, 1 otherwise.
pub async fn run() -> u8 {
    let addr = std::env::var("HTTP_ADDR").unwrap_or_else(|_| DEFAULT_HTTP_ADDR.to_owned());
    let addr = loopback(&addr);

    match tokio::time::timeout(TIMEOUT, probe(&addr)).await {
        Ok(Ok(())) => 0,
        Ok(Err(message)) => {
            eprintln!("healthcheck: {message}");
            1
        }
        Err(_) => {
            eprintln!("healthcheck: the server did not answer within {TIMEOUT:?}");
            1
        }
    }
}

/// Rewrites a wildcard bind address to the loopback. A server listening on
/// every interface is reached over loopback, which is the only interface a
/// probe inside the container can rely on.
fn loopback(addr: &str) -> String {
    match addr.rsplit_once(':') {
        Some((host, port)) if host.is_empty() || host == "0.0.0.0" => format!("127.0.0.1:{port}"),
        Some((host, port)) if host == "[::]" || host == "::" => format!("[::1]:{port}"),
        _ => addr.to_owned(),
    }
}

async fn probe(addr: &str) -> Result<(), String> {
    let mut stream = TcpStream::connect(addr)
        .await
        // The message names the failure, never the address: the address is
        // this container's own and saying it adds nothing.
        .map_err(|_| "the server is not accepting connections".to_owned())?;

    let request = "GET /health HTTP/1.1\r\nHost: localhost\r\nConnection: close\r\n\r\n";
    stream
        .write_all(request.as_bytes())
        .await
        .map_err(|_| "the request could not be sent".to_owned())?;

    // The status line is all that matters, and it arrives first.
    let mut response = [0_u8; 15];
    stream
        .read_exact(&mut response)
        .await
        .map_err(|_| "the server closed the connection".to_owned())?;

    let status = String::from_utf8_lossy(&response);
    if status.starts_with("HTTP/1.1 200") {
        return Ok(());
    }
    Err(format!("the server answered {}", status.trim()))
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn a_wildcard_bind_is_probed_over_the_loopback() {
        assert_eq!(loopback("0.0.0.0:8081"), "127.0.0.1:8081");
        assert_eq!(loopback(":8081"), "127.0.0.1:8081");
        assert_eq!(loopback("[::]:8081"), "[::1]:8081");
        // An address already naming an interface is left alone.
        assert_eq!(loopback("127.0.0.1:9000"), "127.0.0.1:9000");
        assert_eq!(loopback("10.0.0.5:9000"), "10.0.0.5:9000");
    }

    #[tokio::test]
    async fn a_server_that_is_not_there_fails_rather_than_hanging() {
        // Port 1 accepts nothing.
        let result = probe("127.0.0.1:1").await;
        assert!(result.is_err(), "a dead server reported healthy");
        let message = result.unwrap_err();
        assert!(
            !message.contains("127.0.0.1"),
            "the message echoed the address: {message}"
        );
    }

    #[tokio::test]
    async fn a_server_answering_200_is_healthy_and_anything_else_is_not() {
        for (status, healthy) in [("200 OK", true), ("503 Service Unavailable", false)] {
            let listener = tokio::net::TcpListener::bind("127.0.0.1:0").await.unwrap();
            let addr = listener.local_addr().unwrap().to_string();
            let response = format!("HTTP/1.1 {status}\r\ncontent-length: 0\r\n\r\n");
            tokio::spawn(async move {
                let (mut socket, _) = listener.accept().await.unwrap();
                let mut discard = [0_u8; 1024];
                let _ = socket.read(&mut discard).await;
                let _ = socket.write_all(response.as_bytes()).await;
            });

            assert_eq!(
                probe(&addr).await.is_ok(),
                healthy,
                "a server answering {status} was judged wrongly"
            );
        }
    }
}
