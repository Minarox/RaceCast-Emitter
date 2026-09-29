//! GPS telemetry from the modem GNSS. The program enables the GNSS through ModemManager in "unmanaged"
//! mode (the NMEA port stays free), finds the NMEA port from ModemManager (robust to `ttyUSB` renumbering)
//! and reads the sentences itself. GPS is optional: no fix, no port or no modem is a normal state (empty
//! fields, `info` log once a fix is gained or lost for good), nothing else depends on it. With a fix, the
//! GNSS time also feeds chrony (`chrony`), as a time source when there is no network.

use std::fs::OpenOptions;
use std::io::Read;
use std::os::unix::fs::OpenOptionsExt;
use std::time::{Duration, Instant, SystemTime};

use rustix::termios::{self, OptionalActions, SpecialCodeIndex};
use tokio::sync::mpsc;
use tokio_util::sync::CancellationToken;

use super::chrony::Chrony;
use super::mm::{LOCATION, LOCATION_3GPP, LOCATION_GPS_UNMANAGED, ModemManager};
use super::nmea::{self, GpsState};
use super::{CsvRecorder, Output};

/// No NMEA sentence for this long: the port is reopened.
const NMEA_TIMEOUT: Duration = Duration::from_secs(10);
/// Delay between attempts to find the modem / NMEA port.
const DISCOVERY_RETRY: Duration = Duration::from_secs(10);
/// A fix gained or lost is logged only once the new state has lasted this long: with a weak sky view the fix
/// comes and goes every few seconds (1355 log lines in one night otherwise). 2D ↔ 3D changes are not logged.
const FIX_LOG_STABLE: Duration = Duration::from_secs(10);

/// Samples the GPS every `period` until `token` is cancelled.
pub async fn run(period: Duration, out: Output, token: CancellationToken) -> Result<(), String> {
    let mm = ModemManager::connect().await.map_err(|e| format!("system D-Bus unavailable: {e}"))?;
    let mut csv = CsvRecorder::new("gps", nmea::COLUMNS, out);
    let mut tick = tokio::time::interval(period);
    tick.set_missed_tick_behavior(tokio::time::MissedTickBehavior::Delay);
    let mut setup_warned = false;
    let mut fix_log = FixLog::default();
    loop {
        if let Some(port) = nmea_port(&mm, &mut setup_warned).await {
            tracing::info!(%port, "reading NMEA sentences");
            read_port(&port, &mut csv, &mut tick, &mut fix_log, &token).await;
        }
        // No port, or port lost: keep writing "no fix" lines, then look again.
        let retry_at = Instant::now() + DISCOVERY_RETRY;
        while Instant::now() < retry_at {
            tokio::select! {
                _ = tick.tick() => {
                    let now = Instant::now();
                    fix_log.update("none", now);
                    csv.record(&GpsState::default().row(now));
                }
                () = token.cancelled() => return Ok(()),
            }
        }
    }
}

/// NMEA port of the modem, after making sure the GNSS runs in unmanaged mode. `None` if there is no modem
/// or no NMEA port (logged at `info`, GPS being optional).
async fn nmea_port(mm: &ModemManager, setup_warned: &mut bool) -> Option<String> {
    let modem = match mm.modem().await {
        Ok(Some(m)) => m,
        Ok(None) => {
            tracing::info!("no modem: GPS unavailable for now");
            return None;
        }
        Err(e) => {
            tracing::info!(error = %e, "ModemManager unreachable: GPS unavailable for now");
            return None;
        }
    };
    let enabled = modem.u32(LOCATION, "Enabled").unwrap_or(0);
    if enabled & LOCATION_GPS_UNMANAGED == 0 {
        match mm.enable_location(modem.path.as_str(), enabled, LOCATION_GPS_UNMANAGED | LOCATION_3GPP).await {
            Ok(()) => tracing::info!("modem GNSS enabled (unmanaged mode)"),
            Err(e) if !std::mem::replace(setup_warned, true) => {
                tracing::warn!(error = %e, "cannot enable the modem GNSS (not authorized?)");
            }
            Err(_) => {}
        }
    }
    let port = modem.gps_port();
    if port.is_none() {
        tracing::info!("modem has no NMEA port: GPS unavailable for now");
    }
    port
}

/// Reads `port` until it stops producing sentences, writing one CSV line per `tick`.
async fn read_port(
    port: &str,
    csv: &mut CsvRecorder,
    tick: &mut tokio::time::Interval,
    fix_log: &mut FixLog,
    token: &CancellationToken,
) {
    let (tx, mut rx) = mpsc::channel::<(String, SystemTime)>(64);
    // The reader thread stops when this function returns (whatever the reason).
    let reader_token = token.child_token();
    let _stop_reader = reader_token.clone().drop_guard();
    let path = port.to_string();
    let reader = tokio::task::spawn_blocking(move || read_lines(&path, &tx, &reader_token));

    let mut state = GpsState::default();
    let mut chrony = Chrony::default();
    let mut last_line = Instant::now();
    loop {
        tokio::select! {
            line = rx.recv() => match line {
                Some((line, received)) => {
                    last_line = Instant::now();
                    if let Some(sentence) = nmea::parse(&line) {
                        if let Some(time) = sentence.fix_time() {
                            chrony.send(received, time);
                        }
                        state.update(sentence, last_line);
                    }
                }
                None => {
                    // Reader stopped: report why, then let the caller rediscover the port.
                    let why = match reader.await {
                        Ok(Ok(())) => "port closed".to_string(),
                        Ok(Err(e)) => e.to_string(),
                        Err(e) => e.to_string(),
                    };
                    tracing::info!(%port, reason = %why, "NMEA port lost");
                    return;
                }
            },
            _ = tick.tick() => {
                let now = Instant::now();
                let fix = state.fix(now);
                fix_log.update(fix, now);
                csv.record(&state.row(now));
                if now.duration_since(last_line) > NMEA_TIMEOUT {
                    tracing::info!(%port, timeout_s = NMEA_TIMEOUT.as_secs(), "no NMEA sentence, reopening the port");
                    return;
                }
            }
            () = token.cancelled() => return,
        }
    }
}

/// Logs the fix being gained or lost, once the new state is stable ([`FIX_LOG_STABLE`]).
#[derive(Debug, Default)]
struct FixLog {
    /// Last state logged (starts without a fix).
    reported: bool,
    /// When the current, not yet logged, state began.
    since: Option<Instant>,
}

impl FixLog {
    fn update(&mut self, fix: &str, now: Instant) {
        match self.change(fix != "none", now) {
            Some(true) => tracing::info!(fix, stable_s = FIX_LOG_STABLE.as_secs(), "GPS fix acquired"),
            Some(false) => tracing::info!(stable_s = FIX_LOG_STABLE.as_secs(), "GPS fix lost"),
            None => {}
        }
    }

    /// The new state, once it has lasted [`FIX_LOG_STABLE`].
    fn change(&mut self, has_fix: bool, now: Instant) -> Option<bool> {
        if has_fix == self.reported {
            self.since = None;
            return None;
        }
        let since = *self.since.get_or_insert(now);
        if now.duration_since(since) < FIX_LOG_STABLE {
            return None;
        }
        self.reported = has_fix;
        self.since = None;
        Some(has_fix)
    }
}

/// Blocking reader: opens the tty in raw mode (no echo back to the modem) with a 1 s read timeout, so that
/// cancellation is checked regularly, and forwards complete lines with the local time they were read at
/// (the reference of the chrony samples).
fn read_lines(path: &str, tx: &mpsc::Sender<(String, SystemTime)>, token: &CancellationToken) -> std::io::Result<()> {
    let mut file = OpenOptions::new().read(true).custom_flags(rustix::fs::OFlags::NOCTTY.bits() as i32).open(path)?;
    let mut tio = termios::tcgetattr(&file)?;
    tio.make_raw();
    tio.special_codes[SpecialCodeIndex::VMIN] = 0;
    tio.special_codes[SpecialCodeIndex::VTIME] = 10;
    termios::tcsetattr(&file, OptionalActions::Now, &tio)?;

    let mut buf = [0u8; 1024];
    let mut pending: Vec<u8> = Vec::new();
    while !token.is_cancelled() {
        let n = match file.read(&mut buf) {
            Ok(n) => n,
            Err(e) if e.kind() == std::io::ErrorKind::Interrupted => continue,
            Err(e) => return Err(e),
        };
        let received = SystemTime::now();
        pending.extend_from_slice(&buf[..n]);
        while let Some(end) = pending.iter().position(|&b| b == b'\n') {
            let line: Vec<u8> = pending.drain(..=end).collect();
            let line = String::from_utf8_lossy(&line).trim().to_string();
            if !line.is_empty() && tx.try_send((line, received)).is_err() && tx.is_closed() {
                return Ok(());
            }
        }
        if pending.len() > 4096 {
            pending.clear(); // garbage without newlines
        }
    }
    Ok(())
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn flapping_fix_is_not_logged() {
        let t0 = Instant::now();
        let mut log = FixLog::default();
        // Fix for 4 s, none for 3 s, over and over (weak sky view).
        for s in 0..120 {
            let has_fix = s % 7 < 4;
            assert_eq!(log.change(has_fix, t0 + Duration::from_secs(s)), None, "second {s}");
        }
    }

    #[test]
    fn stable_changes_are_logged_once() {
        let t0 = Instant::now();
        let at = |s: u64| t0 + Duration::from_secs(s);
        let mut log = FixLog::default();
        assert_eq!(log.change(true, at(0)), None);
        assert_eq!(log.change(true, at(9)), None);
        assert_eq!(log.change(true, at(10)), Some(true));
        assert_eq!(log.change(true, at(11)), None);
        assert_eq!(log.change(false, at(20)), None);
        assert_eq!(log.change(false, at(30)), Some(false));
        assert_eq!(log.change(false, at(60)), None);
    }
}
