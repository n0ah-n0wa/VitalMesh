//! Request and correlation identifiers.
//!
//! A request id identifies one hop; a correlation id follows a client
//! request across every service it touches. Both are validated before being
//! echoed, so a malformed client value can never reach headers or logs.

use std::hash::{BuildHasher, Hasher, RandomState};
use std::sync::atomic::{AtomicU64, Ordering};
use std::time::{SystemTime, UNIX_EPOCH};

/// Header carrying the per-hop request id in both directions.
pub const REQUEST_ID_HEADER: &str = "x-request-id";

/// Header carrying the end-to-end correlation id in both directions.
pub const CORRELATION_ID_HEADER: &str = "x-correlation-id";

const MAX_LEN: usize = 128;

static SEQUENCE: AtomicU64 = AtomicU64::new(0);

/// Whether `id` is acceptable as a client-supplied identifier: 1 to 128
/// characters from `[A-Za-z0-9._-]`.
pub fn is_valid(id: &str) -> bool {
    !id.is_empty()
        && id.len() <= MAX_LEN
        && id
            .bytes()
            .all(|b| b.is_ascii_alphanumeric() || matches!(b, b'.' | b'_' | b'-'))
}

/// Generates a new identifier: 32 lowercase hexadecimal characters derived
/// from the clock, a process-wide sequence and per-process random hash keys.
/// It is unique for practical purposes but is an identifier, not a secret.
pub fn generate() -> String {
    let nanos = SystemTime::now()
        .duration_since(UNIX_EPOCH)
        .map(|d| d.as_nanos() as u64)
        .unwrap_or_default();
    let sequence = SEQUENCE.fetch_add(1, Ordering::Relaxed);

    let mut high = RandomState::new().build_hasher();
    high.write_u64(nanos);
    high.write_u64(sequence);
    let mut low = RandomState::new().build_hasher();
    low.write_u64(sequence);
    low.write_u64(nanos);

    format!("{:016x}{:016x}", high.finish(), low.finish())
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::collections::HashSet;

    #[test]
    fn validation() {
        assert!(is_valid("abc"));
        assert!(is_valid("req-123_x.y"));
        assert!(is_valid(&"a".repeat(128)));
        for bad in [
            "",
            "has space",
            "semi;colon",
            "new\nline",
            "ünï",
            &"a".repeat(129),
        ] {
            assert!(!is_valid(bad), "{bad:?}");
        }
    }

    #[test]
    fn generated_ids_are_valid_and_unique() {
        let ids: HashSet<String> = (0..1000).map(|_| generate()).collect();
        assert_eq!(ids.len(), 1000);
        for id in &ids {
            assert!(is_valid(id));
            assert_eq!(id.len(), 32);
            assert!(id.bytes().all(|b| b.is_ascii_hexdigit()));
        }
    }
}
