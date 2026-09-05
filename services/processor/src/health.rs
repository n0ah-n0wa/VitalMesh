//! Liveness and readiness endpoints.

use axum::Json;
use serde::Serialize;

use crate::{SERVICE_NAME, VERSION};

/// JSON body returned by the health endpoints.
#[derive(Debug, Serialize)]
#[cfg_attr(test, derive(PartialEq, Eq))]
pub struct Health {
    pub status: &'static str,
    pub service: &'static str,
    pub version: &'static str,
}

impl Health {
    fn ok() -> Self {
        Self {
            status: "ok",
            service: SERVICE_NAME,
            version: VERSION,
        }
    }
}

/// Process liveness. Must never depend on external systems.
pub async fn live() -> Json<Health> {
    Json(Health::ok())
}

/// Readiness to accept work. No external dependencies are wired yet, so the
/// service is ready as soon as it accepts connections. Dependency checks are
/// added together with the PostgreSQL integration.
pub async fn ready() -> Json<Health> {
    Json(Health::ok())
}

#[cfg(test)]
mod tests {
    use super::*;

    #[tokio::test]
    async fn live_and_ready_report_ok() {
        let expected = Health {
            status: "ok",
            service: SERVICE_NAME,
            version: VERSION,
        };
        assert_eq!(live().await.0, expected);
        assert_eq!(ready().await.0, expected);
    }
}
