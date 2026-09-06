//! Bounded admission control. Work that cannot obtain a permit is rejected
//! immediately so that no unbounded queue can form.

use std::num::NonZeroUsize;

use tokio::sync::{Semaphore, SemaphorePermit, TryAcquireError};

use crate::error::{Error, Kind, Result};

/// Limits how many units of work run at the same time.
#[derive(Debug)]
pub struct Limiter {
    semaphore: Semaphore,
    capacity: usize,
}

/// Proof that a unit of work was admitted. Capacity is returned when it drops.
#[derive(Debug)]
pub struct Permit<'a> {
    _permit: SemaphorePermit<'a>,
}

impl Limiter {
    pub fn new(capacity: NonZeroUsize) -> Self {
        Self {
            semaphore: Semaphore::new(capacity.get()),
            capacity: capacity.get(),
        }
    }

    /// Admits one unit of work, or fails with an overloaded error when the
    /// limiter is at capacity. It never waits.
    pub fn try_acquire(&self) -> Result<Permit<'_>> {
        match self.semaphore.try_acquire() {
            Ok(permit) => Ok(Permit { _permit: permit }),
            Err(TryAcquireError::NoPermits) => Err(Error::overloaded()),
            Err(TryAcquireError::Closed) => Err(Error::new(
                Kind::Unavailable,
                "PROCESSOR_SHUTTING_DOWN",
                "The processor is shutting down.",
            )),
        }
    }

    pub fn capacity(&self) -> usize {
        self.capacity
    }

    pub fn available(&self) -> usize {
        self.semaphore.available_permits()
    }

    pub fn active(&self) -> usize {
        self.capacity - self.available()
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    fn limiter(n: usize) -> Limiter {
        Limiter::new(NonZeroUsize::new(n).unwrap())
    }

    #[test]
    fn rejects_beyond_capacity_and_releases_on_drop() {
        let l = limiter(2);
        assert_eq!((l.capacity(), l.available(), l.active()), (2, 2, 0));

        let a = l.try_acquire().expect("first permit");
        let b = l.try_acquire().expect("second permit");
        assert_eq!(l.active(), 2);

        let err = l.try_acquire().expect_err("third must be rejected");
        assert_eq!(err.kind(), Kind::Overloaded);
        assert_eq!(err.code(), "PROCESSOR_OVERLOADED");

        drop(a);
        assert_eq!(l.available(), 1);
        let _c = l.try_acquire().expect("capacity returned");
        drop(b);
        assert_eq!(l.active(), 1);
    }
}
