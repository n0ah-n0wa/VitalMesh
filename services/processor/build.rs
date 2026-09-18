//! Records what built this binary, so the running service can say it.
//!
//! Two facts are not available to Rust at run time and have to be captured
//! while compiling: the toolchain version, and the identity of the locked
//! dependency set. Both are emitted as compile-time environment variables
//! that `lib.rs` reads.
//!
//! This script deliberately pulls in no crates. A build dependency added
//! for build metadata would appear in `Cargo.lock`, be scanned as part of
//! the supply chain, and need updating for ever — a poor trade for a hash.
//! The consequence is that the lockfile identifier below is FNV-1a rather
//! than SHA-256, and it is named `fnv64:` so nobody mistakes it for a
//! cryptographic digest. Its job is to answer "is this the same dependency
//! set?", not to resist an adversary; the supply chain is protected by
//! `Cargo.lock` itself, `--locked`, and the scanners.

use std::path::Path;
use std::process::Command;

fn main() {
    println!("cargo:rerun-if-changed=build.rs");
    println!("cargo:rerun-if-changed=Cargo.lock");

    println!("cargo:rustc-env=VITALMESH_RUST_VERSION={}", rust_version());
    println!(
        "cargo:rustc-env=VITALMESH_CARGO_LOCK={}",
        cargo_lock_identity()
    );
}

/// The compiler's own version string, as `rustc -V` reports it. Cargo sets
/// RUSTC to the compiler it is using, so this is the one that built this.
fn rust_version() -> String {
    let rustc = std::env::var("RUSTC").unwrap_or_else(|_| "rustc".to_owned());
    let output = match Command::new(rustc).arg("-V").output() {
        Ok(output) if output.status.success() => output.stdout,
        _ => return "unknown".to_owned(),
    };
    match String::from_utf8(output) {
        // "rustc 1.89.0 (abcdef 2025-01-01)" -> as printed, trimmed.
        Ok(text) => {
            let trimmed = text.trim();
            if trimmed.is_empty() {
                "unknown".to_owned()
            } else {
                sanitise(trimmed)
            }
        }
        Err(_) => "unknown".to_owned(),
    }
}

/// An identifier for the locked dependency set: a 64-bit FNV-1a hash of
/// Cargo.lock. Two builds reporting the same value locked the same crates.
fn cargo_lock_identity() -> String {
    let lock = Path::new("Cargo.lock");
    let Ok(bytes) = std::fs::read(lock) else {
        return "unknown".to_owned();
    };
    // FNV-1a, 64-bit. Eight lines, no dependency, and deterministic across
    // platforms because it reads the bytes as they are.
    let mut hash: u64 = 0xcbf2_9ce4_8422_2325;
    for byte in &bytes {
        hash ^= u64::from(*byte);
        hash = hash.wrapping_mul(0x0000_0100_0000_01b3);
    }
    format!("fnv64:{hash:016x}")
}

/// Keeps the value usable as an environment variable and as a metric label:
/// no newlines, no quotes.
fn sanitise(value: &str) -> String {
    value
        .chars()
        .map(|c| match c {
            '\n' | '\r' | '"' | '\\' => ' ',
            other => other,
        })
        .collect::<String>()
        .trim()
        .to_owned()
}
