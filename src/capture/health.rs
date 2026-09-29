//! Health of a device task, watched by the manager to confirm a configuration change (SPEC §8): a new
//! configuration is only kept if the restarted devices produce frames for a few seconds without any
//! failure.
//!
//! The device task reports its capture start, every frame (from the streaming thread: one atomic store)
//! and every failure (pipeline error, recording or stream branch error, task error). One `Health` per
//! spawned task: it covers the supervisor's restarts of that task, so a failure followed by a successful
//! restart still counts as a failure.

use std::sync::atomic::{AtomicU64, Ordering};
use std::sync::{Arc, Mutex};
use std::time::{Duration, Instant};

/// Frames must still be arriving this recently for the device to be judged healthy.
const MAX_FRAME_GAP: Duration = Duration::from_secs(1);

#[derive(Debug, Clone, PartialEq, Eq)]
pub enum Verdict {
    /// Not started yet, or not running for long enough.
    Pending,
    /// Frames flowing for at least the confirmation window, no failure.
    Healthy,
    Failed(String),
}

#[derive(Clone)]
pub struct Health {
    inner: Arc<Inner>,
}

struct Inner {
    epoch: Instant,
    /// Milliseconds since `epoch` (0 = never).
    started_ms: AtomicU64,
    last_frame_ms: AtomicU64,
    failure: Mutex<Option<String>>,
}

impl Default for Health {
    fn default() -> Self {
        Self {
            inner: Arc::new(Inner {
                epoch: Instant::now(),
                started_ms: AtomicU64::new(0),
                last_frame_ms: AtomicU64::new(0),
                failure: Mutex::new(None),
            }),
        }
    }
}

impl Health {
    fn now_ms(&self) -> u64 {
        self.inner.epoch.elapsed().as_millis().max(1) as u64
    }

    /// The capture delivers its first frames.
    pub fn started(&self) {
        self.inner.started_ms.store(self.now_ms(), Ordering::Relaxed);
    }

    /// A frame (or audio buffer) went through. Cheap: called from the streaming thread.
    pub fn frame(&self) {
        self.inner.last_frame_ms.store(self.now_ms(), Ordering::Relaxed);
    }

    /// Something failed; the first failure is kept.
    pub fn failed(&self, error: &str) {
        let mut failure = self.inner.failure.lock().unwrap_or_else(std::sync::PoisonError::into_inner);
        failure.get_or_insert_with(|| error.to_string());
    }

    pub fn verdict(&self, window: Duration) -> Verdict {
        let failure = self.inner.failure.lock().unwrap_or_else(std::sync::PoisonError::into_inner).clone();
        verdict(
            self.now_ms(),
            self.inner.started_ms.load(Ordering::Relaxed),
            self.inner.last_frame_ms.load(Ordering::Relaxed),
            failure,
            window,
        )
    }
}

fn verdict(now_ms: u64, started_ms: u64, last_frame_ms: u64, failure: Option<String>, window: Duration) -> Verdict {
    if let Some(error) = failure {
        return Verdict::Failed(error);
    }
    let running_for = now_ms.saturating_sub(started_ms);
    let frame_age = now_ms.saturating_sub(last_frame_ms);
    let flowing = last_frame_ms > 0 && frame_age <= MAX_FRAME_GAP.as_millis() as u64;
    if started_ms > 0 && running_for >= window.as_millis() as u64 && flowing {
        Verdict::Healthy
    } else {
        Verdict::Pending
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    const W: Duration = Duration::from_secs(5);

    #[test]
    fn healthy_only_after_the_window_with_frames_flowing() {
        assert_eq!(verdict(3_000, 0, 0, None, W), Verdict::Pending); // not started
        assert_eq!(verdict(6_000, 2_000, 5_900, None, W), Verdict::Pending); // 4 s only
        assert_eq!(verdict(7_000, 2_000, 6_900, None, W), Verdict::Healthy);
        // Started long ago but frames stopped: not healthy (a stall ends as a failure a bit later).
        assert_eq!(verdict(9_000, 2_000, 7_000, None, W), Verdict::Pending);
    }

    #[test]
    fn any_failure_wins() {
        let failed = verdict(20_000, 2_000, 19_990, Some("recording branch failed".into()), W);
        assert_eq!(failed, Verdict::Failed("recording branch failed".into()));
    }

    #[test]
    fn first_failure_is_kept() {
        let health = Health::default();
        health.started();
        health.failed("first");
        health.failed("second");
        assert_eq!(health.verdict(W), Verdict::Failed("first".into()));
    }
}
