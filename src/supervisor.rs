//! Supervision: every task runs in isolation and is restarted with backoff if it fails or panics, without
//! ever stopping the program.

use std::collections::BTreeMap;
use std::fmt::Display;
use std::future::Future;
use std::sync::{Arc, Mutex};
use std::time::Duration;

use tokio::task::JoinHandle;
use tokio::time::Instant;
use tokio_util::sync::CancellationToken;
use tokio_util::task::TaskTracker;
use tracing::Instrument;

/// What to do when a task ends without an error.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum Restart {
    /// Task meant to run forever: a normal end is abnormal, restart it.
    Always,
    /// Task tied to a resource (e.g. an unplugged device): a normal end is final.
    OnError,
}

/// Exponential backoff, reset when the task has run long enough without failing.
#[derive(Debug, Clone)]
pub struct Backoff {
    initial: Duration,
    max: Duration,
    stable_after: Duration,
    next: Duration,
}

impl Default for Backoff {
    fn default() -> Self {
        Self::new(Duration::from_secs(1), Duration::from_secs(60), Duration::from_secs(60))
    }
}

impl Backoff {
    pub fn new(initial: Duration, max: Duration, stable_after: Duration) -> Self {
        Self { initial, max, stable_after, next: initial }
    }

    /// Delay before the next restart of a task that ran for `ran`.
    pub fn delay_after(&mut self, ran: Duration) -> Duration {
        if ran >= self.stable_after {
            self.next = self.initial;
        }
        let delay = self.next;
        self.next = (self.next * 2).min(self.max);
        delay
    }
}

#[derive(Clone)]
pub struct Supervisor {
    shutdown: CancellationToken,
    tracker: TaskTracker,
    /// Supervised tasks still running, by name (reported when a shutdown times out).
    running: Arc<Mutex<BTreeMap<String, usize>>>,
}

/// Counts a supervised task as running while alive.
struct Running {
    running: Arc<Mutex<BTreeMap<String, usize>>>,
    name: String,
}

impl Running {
    fn new(running: &Arc<Mutex<BTreeMap<String, usize>>>, name: &str) -> Self {
        *running.lock().unwrap_or_else(std::sync::PoisonError::into_inner).entry(name.to_string()).or_default() += 1;
        Self { running: running.clone(), name: name.to_string() }
    }
}

impl Drop for Running {
    fn drop(&mut self) {
        let mut map = self.running.lock().unwrap_or_else(std::sync::PoisonError::into_inner);
        if let Some(n) = map.get_mut(&self.name) {
            *n -= 1;
            if *n == 0 {
                map.remove(&self.name);
            }
        }
    }
}

impl Supervisor {
    pub fn new(shutdown: CancellationToken) -> Self {
        Self { shutdown, tracker: TaskTracker::new(), running: Arc::default() }
    }

    /// Runs `name` under supervision until program shutdown. `factory` creates a new instance of the task
    /// on every (re)start; the task must stop cleanly when the token it receives is cancelled. The task's
    /// logs carry the `task` field (e.g. `camera:front`, `gps`).
    pub fn spawn<F, Fut, E>(&self, name: impl Into<String>, restart: Restart, factory: F) -> JoinHandle<()>
    where
        F: FnMut(CancellationToken) -> Fut + Send + 'static,
        Fut: Future<Output = Result<(), E>> + Send + 'static,
        E: Display + Send + 'static,
    {
        self.spawn_scoped(name, restart, self.shutdown.clone(), factory)
    }

    /// Same as [`Supervisor::spawn`], but the task (and its restarts) also stops when `scope` is cancelled,
    /// e.g. when its device is unplugged. `scope` must be a child of the shutdown token. The returned handle
    /// completes once the task is fully stopped.
    pub fn spawn_scoped<F, Fut, E>(
        &self,
        name: impl Into<String>,
        restart: Restart,
        scope: CancellationToken,
        factory: F,
    ) -> JoinHandle<()>
    where
        F: FnMut(CancellationToken) -> Fut + Send + 'static,
        Fut: Future<Output = Result<(), E>> + Send + 'static,
        E: Display + Send + 'static,
    {
        self.spawn_inner(name.into(), restart, scope, Backoff::default(), factory)
    }

    /// Same as [`Supervisor::spawn`] with a custom backoff (e.g. a shorter maximum delay for the LiveKit
    /// connection, so that streaming comes back quickly after a tunnel).
    pub fn spawn_with_backoff<F, Fut, E>(
        &self,
        name: impl Into<String>,
        restart: Restart,
        backoff: Backoff,
        factory: F,
    ) -> JoinHandle<()>
    where
        F: FnMut(CancellationToken) -> Fut + Send + 'static,
        Fut: Future<Output = Result<(), E>> + Send + 'static,
        E: Display + Send + 'static,
    {
        self.spawn_inner(name.into(), restart, self.shutdown.clone(), backoff, factory)
    }

    fn spawn_inner<F, Fut, E>(
        &self,
        name: String,
        restart: Restart,
        scope: CancellationToken,
        mut backoff: Backoff,
        mut factory: F,
    ) -> JoinHandle<()>
    where
        F: FnMut(CancellationToken) -> Fut + Send + 'static,
        Fut: Future<Output = Result<(), E>> + Send + 'static,
        E: Display + Send + 'static,
    {
        let shutdown = scope;
        let span = tracing::info_span!("task", task = %name);
        let running = Running::new(&self.running, &name);
        self.tracker.spawn(
            async move {
                let _running = running;
                while !shutdown.is_cancelled() {
                    let started = Instant::now();
                    let instance = tokio::spawn(factory(shutdown.child_token()).in_current_span());
                    let failed = match instance.await {
                        Ok(Ok(())) if shutdown.is_cancelled() => break,
                        Ok(Ok(())) if restart == Restart::OnError => {
                            tracing::info!("task finished");
                            break;
                        }
                        Ok(Ok(())) => {
                            tracing::warn!("task ended unexpectedly");
                            false
                        }
                        Ok(Err(e)) => {
                            tracing::warn!(error = %e, "task failed");
                            true
                        }
                        Err(e) if e.is_panic() => {
                            tracing::error!("task aborted by a panic");
                            true
                        }
                        Err(e) => {
                            tracing::error!(error = %e, "task cancelled");
                            true
                        }
                    };
                    let delay = backoff.delay_after(started.elapsed());
                    tracing::info!(delay_ms = delay.as_millis() as u64, after_failure = failed, "restart scheduled");
                    tokio::select! {
                        () = tokio::time::sleep(delay) => {}
                        () = shutdown.cancelled() => break,
                    }
                }
            }
            .instrument(span),
        )
    }

    /// Waits for every task to finish (after the token is cancelled), for at most `timeout`. Returns the
    /// names of the tasks still running (empty if all stopped).
    pub async fn wait(&self, timeout: Duration) -> Vec<String> {
        self.tracker.close();
        if tokio::time::timeout(timeout, self.tracker.wait()).await.is_ok() {
            return Vec::new();
        }
        self.running.lock().unwrap_or_else(std::sync::PoisonError::into_inner).keys().cloned().collect()
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::sync::Arc;
    use std::sync::atomic::{AtomicU32, Ordering};

    #[test]
    fn backoff_grows_then_resets() {
        let mut b = Backoff::new(Duration::from_secs(1), Duration::from_secs(8), Duration::from_secs(60));
        let short = Duration::from_millis(10);
        let delays: Vec<u64> = (0..6).map(|_| b.delay_after(short).as_secs()).collect();
        assert_eq!(delays, [1, 2, 4, 8, 8, 8]);
        assert_eq!(b.delay_after(Duration::from_secs(61)).as_secs(), 1);
        assert_eq!(b.delay_after(short).as_secs(), 2);
    }

    #[tokio::test(start_paused = true)]
    async fn restarts_after_panic_and_error_then_stops_on_shutdown() {
        let shutdown = CancellationToken::new();
        let sup = Supervisor::new(shutdown.clone());
        let runs = Arc::new(AtomicU32::new(0));
        let counter = runs.clone();
        sup.spawn("test", Restart::Always, move |token| {
            let n = counter.fetch_add(1, Ordering::SeqCst);
            async move {
                match n {
                    0 => panic!("deliberate panic"),
                    1 => Err("deliberate failure"),
                    _ => {
                        token.cancelled().await;
                        Ok(())
                    }
                }
            }
        });
        tokio::time::sleep(Duration::from_secs(10)).await;
        assert_eq!(runs.load(Ordering::SeqCst), 3);
        shutdown.cancel();
        assert!(sup.wait(Duration::from_secs(1)).await.is_empty());
        assert_eq!(runs.load(Ordering::SeqCst), 3);
    }

    #[tokio::test(start_paused = true)]
    async fn on_error_task_ends_for_good() {
        let shutdown = CancellationToken::new();
        let sup = Supervisor::new(shutdown.clone());
        let runs = Arc::new(AtomicU32::new(0));
        let counter = runs.clone();
        sup.spawn("test", Restart::OnError, move |_| {
            counter.fetch_add(1, Ordering::SeqCst);
            async { Ok::<(), String>(()) }
        });
        tokio::time::sleep(Duration::from_secs(10)).await;
        assert_eq!(runs.load(Ordering::SeqCst), 1);
        assert!(sup.wait(Duration::from_secs(1)).await.is_empty());
    }
}
