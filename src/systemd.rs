//! systemd integration (`Type=notify`): ready/stopping state and watchdog. Without systemd
//! (`NOTIFY_SOCKET` unset), everything is a no-op.

use std::time::Duration;

use sd_notify::NotifyState;
use tokio_util::sync::CancellationToken;

pub fn ready() {
    notify(&[NotifyState::Ready, NotifyState::Status("running")]);
}

pub fn stopping() {
    notify(&[NotifyState::Stopping, NotifyState::Status("shutting down")]);
}

/// Watchdog ping period (half the systemd timeout), if `WatchdogSec=` is enabled.
pub fn watchdog_period() -> Option<Duration> {
    sd_notify::watchdog_enabled().map(|d| d / 2)
}

/// Pings the watchdog as long as the runtime responds. If the program hangs, the pings stop and systemd
/// restarts it. A late ping (runtime blocked for a while, not long enough for a restart) is logged, to
/// spot near misses before they become restarts.
pub async fn watchdog(period: Duration, shutdown: CancellationToken) -> Result<(), String> {
    let mut tick = tokio::time::interval(period);
    tick.set_missed_tick_behavior(tokio::time::MissedTickBehavior::Delay);
    let mut last = tokio::time::Instant::now();
    loop {
        tokio::select! {
            _ = tick.tick() => {
                let gap = last.elapsed();
                last = tokio::time::Instant::now();
                if gap > period * 3 / 2 {
                    tracing::warn!(gap_ms = gap.as_millis() as u64, period_ms = period.as_millis() as u64,
                        "late systemd watchdog ping: the async runtime was blocked");
                }
                notify(&[NotifyState::Watchdog]);
            }
            () = shutdown.cancelled() => return Ok(()),
        }
    }
}

fn notify(state: &[NotifyState]) {
    if let Err(e) = sd_notify::notify(state) {
        tracing::warn!(error = %e, "cannot notify systemd");
    }
}
