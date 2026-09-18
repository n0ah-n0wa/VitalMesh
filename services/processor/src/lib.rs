//! VitalMesh processor: the data processing service behind the API gateway.
//!
//! Module map:
//! - `config`: typed configuration loaded from the environment.
//! - `error`: the classified error type shared by every layer.
//! - `domain`: job identifiers and the job state machine.
//! - `concurrency`: the bounded admission limiter.
//! - `engine`: bounded, cancellable, time-limited job execution.
//! - `jobs`: the bounded registry of jobs this instance is running or ran.
//! - `stats`: the statistical processing engine (descriptive statistics, rolling
//!   statistics, tumbling windows).
//! - `anomaly`: configurable, versioned anomaly detection over window results.
//! - `pipeline`: the end-to-end processing pipeline and its bounded executor.
//! - `state`: application state shared with request handlers.
//! - `telemetry`: structured logging.
//! - `requestid`: request and correlation identifiers.
//! - `transport`: the HTTP router, middleware and handlers.
//! - `lifecycle`: start-up, serving and graceful shutdown.

pub mod anomaly;
pub mod concurrency;
pub mod config;
pub mod domain;
pub mod engine;
pub mod error;
pub mod healthcheck;
pub mod jobs;
pub mod lifecycle;
pub mod metrics;
pub mod pipeline;
mod redact;
pub mod requestid;
pub mod state;
pub mod stats;
pub mod telemetry;
pub mod tracing_otel;
pub mod transport;

/// Service name reported in logs and health responses.
pub const SERVICE_NAME: &str = "processor";

/// The version of `contracts/internal-api/processor-v1.json` this service
/// implements. It is reported by the internal health endpoint so the gateway
/// can detect an incompatible peer, and `tests/contract.rs` fails if it ever
/// disagrees with the document.
pub const CONTRACT_VERSION: &str = "1.1.2";

/// The VERSION an unstamped build carries, and so what a plain
/// `cargo build` produces: a working binary that cannot say which commit
/// it came from. [`Build::incomplete`] treats it as a missing version
/// rather than as a version, because a release reporting it is a release
/// nobody can trace.
pub const UNSTAMPED: &str = "dev";

/// Build identifier, injected by the Makefile through `VITALMESH_VERSION`.
pub const VERSION: &str = match option_env!("VITALMESH_VERSION") {
    Some(version) => version,
    None => UNSTAMPED,
};

/// The Rust toolchain that compiled this binary, captured by `build.rs`
/// because `rustc` leaves no run-time equivalent of Go's `runtime.Version`.
pub const RUST_VERSION: &str = match option_env!("VITALMESH_RUST_VERSION") {
    Some(version) => version,
    None => "unknown",
};

/// An identifier for the locked dependency set this binary was built from:
/// a content hash of `Cargo.lock`, computed by `build.rs`. Two builds
/// reporting the same value resolved the same crates at the same versions.
///
/// It is prefixed `fnv64:` rather than `sha256:` on purpose, and is not a
/// security control; `build.rs` explains why.
pub const CARGO_LOCK: &str = match option_env!("VITALMESH_CARGO_LOCK") {
    Some(digest) => digest,
    None => "unknown",
};

/// The digest of the container image this process is running as, when the
/// deployment supplied one. A process cannot read its own image digest
/// portably; the pipeline deploys by digest, so it passes the value in.
pub const IMAGE_DIGEST_ENV: &str = "IMAGE_DIGEST";

/// The value the base Kubernetes manifests carry, which the deployment
/// replaces with the digest it is deploying. It is treated as no digest at
/// all, because reporting it would be worse than reporting nothing: it
/// looks like an answer. The deployment refuses to apply a manifest where
/// it survives, so this only shows up where the manifests were applied by
/// hand, such as a local cluster.
pub const DIGEST_PLACEHOLDER: &str = "set-by-ci";

/// The digest the deployment supplied, or empty when it supplied nothing
/// usable.
fn image_digest() -> String {
    let digest = std::env::var(IMAGE_DIGEST_ENV).unwrap_or_default();
    let digest = digest.trim();
    if digest == DIGEST_PLACEHOLDER {
        return String::new();
    }
    digest.to_owned()
}

/// The release record for this build: every fact that identifies what is
/// running, in one document. `processor version` prints it as JSON, CI
/// validates it, and the same values are published as `build_info` labels.
///
/// Safe to publish: versions and digests only, never a path, a host or a
/// credential. Nothing here reads configuration, which is what keeps it
/// that way (SPECIFICATIONS.md section 41).
///
/// The field names match the gateway's release record wherever the fact is
/// the same, so one query and one gate cover both services.
#[derive(Debug, Clone, PartialEq, Eq, serde::Serialize)]
pub struct Build {
    /// Which service this is.
    pub service: &'static str,
    /// The git commit the binary was built at.
    pub version: &'static str,
    /// The Rust toolchain that compiled it.
    pub rust_version: &'static str,
    /// The identity of the locked dependency set: a content hash of
    /// `Cargo.lock`. Named as the gateway names its module digest, because
    /// it answers the same question.
    pub dependencies: &'static str,
    /// The processing algorithm this build implements.
    pub algorithm_version: String,
    /// The internal-API contract version it speaks.
    pub contract_version: &'static str,
    /// The container image digest, or empty for a build that is not running
    /// from an image, which is honest: there is no image.
    pub image_digest: String,
}

/// Reads the build metadata of the running process.
pub fn build() -> Build {
    Build {
        service: SERVICE_NAME,
        version: VERSION,
        rust_version: RUST_VERSION,
        dependencies: CARGO_LOCK,
        algorithm_version: anomaly::ALGORITHM_VERSION.to_string(),
        contract_version: CONTRACT_VERSION,
        image_digest: image_digest(),
    }
}

impl Build {
    /// Names the fields that identify nothing, so a release can be rejected
    /// for being unidentifiable rather than shipping with "dev" in it. The
    /// image digest is not required: a binary run outside a container has
    /// no image.
    ///
    /// A CI gate reads this through `processor version`; see
    /// `scripts/release-metadata-check.sh`.
    pub fn incomplete(&self) -> Vec<&'static str> {
        let mut missing = Vec::new();
        if self.version == UNSTAMPED {
            missing.push("version");
        }
        for (field, value) in [
            ("service", self.service),
            ("version", self.version),
            ("rust_version", self.rust_version),
            ("dependencies", self.dependencies),
            ("contract_version", self.contract_version),
        ] {
            if value.trim().is_empty() || value == "unknown" {
                missing.push(field);
            }
        }
        if self.algorithm_version.trim().is_empty() {
            missing.push("algorithm_version");
        }
        missing
    }
}

#[cfg(test)]
mod build_tests {
    use super::*;
    use std::sync::Mutex;

    /// The environment is process-wide and cargo runs tests in threads, so
    /// every test that writes it takes this first.
    static ENV_LOCK: Mutex<()> = Mutex::new(());

    // The point of build metadata is that it identifies the build. A value
    // that is absent, or that says "unknown" in a real build, identifies
    // nothing -- and both failure modes are silent, because a metric with
    // an empty label still scrapes.
    #[test]
    fn every_build_field_is_populated() {
        let build = build();
        assert!(!build.version.is_empty(), "version");
        assert_ne!(build.rust_version, "unknown", "rust version not captured");
        assert!(
            build.rust_version.starts_with("rustc "),
            "rust version should be rustc's own string, got {:?}",
            build.rust_version
        );
        assert_ne!(
            build.dependencies, "unknown",
            "Cargo.lock digest not captured"
        );
        assert!(
            build.dependencies.starts_with("fnv64:"),
            "digest should name its algorithm, got {:?}",
            build.dependencies
        );
        assert_eq!(build.dependencies.len(), "fnv64:".len() + 16, "64-bit hex");
        assert!(!build.algorithm_version.is_empty(), "algorithm version");
        assert_eq!(build.contract_version, CONTRACT_VERSION);
    }

    // A label carrying a newline or a quote corrupts the exposition format
    // for every metric after it, so no build value may contain one.
    #[test]
    fn no_build_value_can_corrupt_the_exposition_format() {
        let build = build();
        for (field, value) in [
            ("version", build.version.to_owned()),
            ("rust_version", build.rust_version.to_owned()),
            ("dependencies", build.dependencies.to_owned()),
            ("algorithm_version", build.algorithm_version.clone()),
            ("contract_version", build.contract_version.to_owned()),
            ("image_digest", build.image_digest.clone()),
        ] {
            for bad in ['\n', '\r', '"', '\\'] {
                assert!(!value.contains(bad), "{field} contains {bad:?}: {value:?}");
            }
        }
    }

    // Build metadata is published; a secret reaching it would be published
    // too. Nothing here reads configuration, and this holds it that way.
    #[test]
    fn build_metadata_carries_nothing_sensitive() {
        let build = build();
        let all = format!(
            "{} {} {} {} {} {}",
            build.version,
            build.rust_version,
            build.dependencies,
            build.algorithm_version,
            build.contract_version,
            build.image_digest
        )
        .to_lowercase();
        for forbidden in [
            "password",
            "secret",
            "token",
            "apikey",
            "api_key",
            "postgres://",
            "redis://",
            "amazonaws.com",
            "/home/",
            "/root/",
            "bearer ",
        ] {
            assert!(
                !all.contains(forbidden),
                "build metadata mentions {forbidden:?}: {all}"
            );
        }
    }

    // The digest identifies the dependency set, so it has to be the same on
    // every read within a build, and it has to be a real hash of the file
    // rather than a constant that happens to look like one.
    // A process cannot read its own image digest, so the deployment
    // supplies it. The manifests' unreplaced placeholder is not a digest:
    // reporting it would be worse than reporting nothing, because it looks
    // like an answer.
    //
    // This test sets a process-wide variable, so it holds the lock the
    // other environment-reading tests use; cargo runs tests in threads.
    #[test]
    fn the_image_digest_comes_from_the_deployment() {
        let _guard = ENV_LOCK.lock().unwrap_or_else(|e| e.into_inner());
        let previous = std::env::var(IMAGE_DIGEST_ENV).ok();

        // SAFETY: the lock above makes this the only thread touching the
        // environment for the length of the test.
        unsafe { std::env::remove_var(IMAGE_DIGEST_ENV) };
        assert_eq!(build().image_digest, "", "with the variable unset");

        let want = "sha256:0b1e5b4c1a2f3d4e5f60718293a4b5c6d7e8f90a1b2c3d4e5f60718293a4b5c6";
        unsafe { std::env::set_var(IMAGE_DIGEST_ENV, format!("  {want}  ")) };
        assert_eq!(build().image_digest, want, "trimmed");

        unsafe { std::env::set_var(IMAGE_DIGEST_ENV, DIGEST_PLACEHOLDER) };
        assert_eq!(
            build().image_digest,
            "",
            "the unreplaced manifest placeholder is not a digest"
        );

        match previous {
            Some(value) => unsafe { std::env::set_var(IMAGE_DIGEST_ENV, value) },
            None => unsafe { std::env::remove_var(IMAGE_DIGEST_ENV) },
        }
    }

    #[test]
    fn the_lockfile_digest_is_stable_and_derived_from_the_file() {
        assert_eq!(build().dependencies, build().dependencies);
        // FNV-1a over an empty input is its offset basis. A digest equal to
        // that means build.rs could not read Cargo.lock.
        assert_ne!(
            CARGO_LOCK, "fnv64:cbf29ce484222325",
            "digest is the hash of no bytes: Cargo.lock was not read"
        );
    }
}
