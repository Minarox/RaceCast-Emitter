//! Modem telemetry (network state, signal, cell) through ModemManager, and the modem restart safeguard
//! (SPEC §9): a modem stuck in the `failed` state for 3 minutes is reset, at most once every 10 minutes.
//! "No network" is a normal state and never triggers anything.

use std::time::{Duration, Instant};

use tokio_util::sync::CancellationToken;

use super::mm::{LOCATION_3GPP, MODEM, MODEM_3GPP, Modem, ModemManager};
use super::{CsvRecorder, Output, Value};

const FAILED_BEFORE_RESET: Duration = Duration::from_secs(180);
const RESET_INTERVAL: Duration = Duration::from_secs(600);

pub const COLUMNS: &[&str] = &[
    "state",
    "access_tech",
    "operator",
    "signal_quality",
    "lte_rssi_dbm",
    "lte_rsrp_dbm",
    "lte_rsrq_db",
    "lte_sinr_db",
    "nr_rsrp_dbm",
    "nr_rsrq_db",
    "nr_sinr_db",
    "cell_id",
    "tac",
    "ip_connected",
];

/// `MMModemState` → name.
pub fn state_name(state: i32) -> &'static str {
    match state {
        -1 => "failed",
        1 => "initializing",
        2 => "locked",
        3 => "disabled",
        4 => "disabling",
        5 => "enabling",
        6 => "enabled",
        7 => "searching",
        8 => "registered",
        9 => "disconnecting",
        10 => "connecting",
        11 => "connected",
        _ => "unknown",
    }
}

/// `MMModemAccessTechnology` bitmask → e.g. `lte+5gnr`.
pub fn access_tech(mask: u32) -> Option<String> {
    const NAMES: [(u32, &str); 11] = [
        (1 << 1, "gsm"),
        (1 << 3, "gprs"),
        (1 << 4, "edge"),
        (1 << 5, "umts"),
        (1 << 6, "hsdpa"),
        (1 << 7, "hsupa"),
        (1 << 8, "hspa"),
        (1 << 9, "hspa+"),
        (1 << 14, "lte"),
        (1 << 15, "5gnr"),
        (1 << 16, "lte-m"),
    ];
    let names: Vec<&str> = NAMES.iter().filter(|(bit, _)| mask & bit != 0).map(|(_, n)| *n).collect();
    (!names.is_empty()).then(|| names.join("+"))
}

/// `MCC,MNC,LAC,CI,TAC` → (cell id, tracking area code), both hexadecimal as reported by the modem.
pub fn cell(location: &str) -> (Option<String>, Option<String>) {
    let f: Vec<&str> = location.split(',').map(str::trim).collect();
    let non_zero = |s: Option<&&str>| s.filter(|s| !s.is_empty() && s.chars().any(|c| c != '0')).map(|s| s.to_string());
    (non_zero(f.get(3)), non_zero(f.get(4)))
}

/// Whether a signal value is physically plausible: the modem reports unknown values as sentinels
/// (-32768 dBm, -3276.8 dB…).
pub fn plausible(metric: &str, v: f64) -> bool {
    let range = match metric {
        "rssi" => -150.0..=0.0,
        "rsrp" => -160.0..=-30.0,
        "rsrq" => -50.0..=20.0,
        "snr" => -30.0..=60.0,
        _ => return v.is_finite(),
    };
    range.contains(&v)
}

/// Decides when to reset a failed modem.
#[derive(Debug, Default)]
pub struct ResetGuard {
    failed_since: Option<Instant>,
    last_reset: Option<Instant>,
}

impl ResetGuard {
    /// Returns `true` when the modem should be reset now.
    pub fn observe(&mut self, failed: bool, now: Instant) -> bool {
        if !failed {
            self.failed_since = None;
            return false;
        }
        let since = *self.failed_since.get_or_insert(now);
        let due = now.duration_since(since) >= FAILED_BEFORE_RESET
            && self.last_reset.is_none_or(|t| now.duration_since(t) >= RESET_INTERVAL);
        if due {
            self.last_reset = Some(now);
            self.failed_since = None;
        }
        due
    }
}

/// Samples the modem every `period` until `token` is cancelled.
pub async fn run(period: Duration, out: Output, token: CancellationToken) -> Result<(), String> {
    let mm = ModemManager::connect().await.map_err(|e| format!("system D-Bus unavailable: {e}"))?;
    let mut csv = CsvRecorder::new("modem", COLUMNS, out);
    let mut guard = ResetGuard::default();
    let mut last_state = String::new();
    let mut warned = Warned::default();
    let mut tick = tokio::time::interval(period);
    tick.set_missed_tick_behavior(tokio::time::MissedTickBehavior::Delay);
    loop {
        tokio::select! {
            _ = tick.tick() => {}
            () = token.cancelled() => return Ok(()),
        }
        let modem = match mm.modem().await {
            Ok(m) => m,
            Err(e) => {
                warn_once(&mut warned.query, || tracing::warn!(error = %e, "ModemManager unreachable"));
                None
            }
        };
        let row = match &modem {
            Some(m) => sample(&mm, m, period, &mut warned).await,
            None => absent_row(),
        };
        let state = match &row[0] {
            Value::Text(Some(s)) => s.clone(),
            _ => String::new(),
        };
        if state != last_state {
            match state.as_str() {
                "failed" | "absent" => tracing::warn!(state, previous = %last_state, "modem state changed"),
                _ => tracing::info!(state, previous = %last_state, "modem state changed"),
            }
            last_state = state.clone();
        }
        csv.record(&row);

        if let Some(m) = &modem
            && guard.observe(state == "failed", Instant::now())
        {
            tracing::warn!("modem failed for 3 minutes: resetting it (streaming and SSH cut for ~30 s)");
            if let Err(e) = mm.reset(m.path.as_str()).await {
                tracing::error!(error = %e, "modem reset refused");
            }
        }
    }
}

/// Warnings already logged (to avoid repeating them every sample).
#[derive(Default)]
struct Warned {
    query: bool,
    signal: bool,
    location: bool,
}

fn warn_once(flag: &mut bool, log: impl FnOnce()) {
    if !std::mem::replace(flag, true) {
        log();
    }
}

fn absent_row() -> Vec<Value> {
    let mut row = vec![Value::text(Some("absent"))];
    row.extend(COLUMNS[1..].iter().map(|_| Value::Text(None)));
    row
}

async fn sample(mm: &ModemManager, m: &Modem, period: Duration, warned: &mut Warned) -> Vec<Value> {
    let path = m.path.as_str();
    // Extended signal values are only refreshed once a rate is set.
    if m.u32(super::mm::SIGNAL, "Rate") == Some(0)
        && let Err(e) = mm.setup_signal(path, period.as_secs().max(1) as u32).await
    {
        warn_once(&mut warned.signal, || {
            tracing::warn!(error = %e, "cannot enable the extended signal values (not authorized?)");
        });
    }
    let (cell_id, tac) = if m.u32(super::mm::LOCATION, "Enabled").is_some_and(|e| e & LOCATION_3GPP != 0) {
        match mm.location_3gpp(path).await {
            Ok(Some(loc)) => cell(&loc),
            Ok(None) => (None, None),
            Err(e) => {
                warn_once(&mut warned.location, || {
                    tracing::warn!(error = %e, "cannot read the cell location (not authorized?)");
                });
                (None, None)
            }
        }
    } else {
        (None, None)
    };
    let mut connected = None;
    for bearer in m.bearers() {
        if let Ok(c) = mm.bearer_connected(&bearer).await {
            connected = Some(connected.unwrap_or(false) || c);
        }
    }
    let lte = m.signal("Lte");
    let nr = m.signal("Nr5g");
    let dbm = |d: &std::collections::HashMap<String, f64>, k: &str| {
        Value::float(d.get(k).copied().filter(|v| plausible(k, *v)), 1)
    };
    vec![
        Value::text(m.i32(MODEM, "State").map(state_name)),
        Value::text(m.u32(MODEM, "AccessTechnologies").and_then(access_tech)),
        Value::text(m.string(MODEM_3GPP, "OperatorName")),
        Value::int(m.signal_quality()),
        dbm(&lte, "rssi"),
        dbm(&lte, "rsrp"),
        dbm(&lte, "rsrq"),
        dbm(&lte, "snr"),
        dbm(&nr, "rsrp"),
        dbm(&nr, "rsrq"),
        dbm(&nr, "snr"),
        Value::text(cell_id),
        Value::text(tac),
        Value::Bool(connected),
    ]
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn names() {
        assert_eq!(state_name(11), "connected");
        assert_eq!(state_name(-1), "failed");
        assert_eq!(state_name(42), "unknown");
        assert_eq!(access_tech((1 << 14) | (1 << 15)).as_deref(), Some("lte+5gnr"));
        assert_eq!(access_tech(0), None);
    }

    #[test]
    fn signal_sentinels_are_dropped() {
        assert!(plausible("rsrp", -90.0));
        assert!(!plausible("rsrp", -32768.0));
        assert!(plausible("snr", 10.8));
        assert!(!plausible("snr", -3276.8));
        assert!(!plausible("rssi", f64::NAN));
    }

    #[test]
    fn cell_location() {
        assert_eq!(cell("208,01,0000,013B2A09,006626"), (Some("013B2A09".into()), Some("006626".into())));
        assert_eq!(cell("208,01,0000,00000000,000000"), (None, None));
        assert_eq!(cell(""), (None, None));
    }

    #[test]
    fn reset_only_after_three_failed_minutes_and_rate_limited() {
        let t0 = Instant::now();
        let at = |s: u64| t0 + Duration::from_secs(s);
        let mut g = ResetGuard::default();
        assert!(!g.observe(true, at(0)));
        assert!(!g.observe(true, at(170)));
        assert!(g.observe(true, at(180)));
        // Still failed right after: wait for both the 3 minutes and the 10-minute interval.
        assert!(!g.observe(true, at(400)));
        assert!(!g.observe(true, at(700)));
        assert!(g.observe(true, at(780)));
        // Recovery clears the failure time.
        assert!(!g.observe(false, at(800)));
        assert!(!g.observe(true, at(1500)));
        assert!(g.observe(true, at(1680)));
    }
}
