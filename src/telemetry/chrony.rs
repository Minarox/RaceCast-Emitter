//! GPS time for chrony (SPEC §7): every valid RMC sentence becomes a sample for chrony's SOCK refclock
//! (`refclock SOCK /run/chrony.racecast.sock`, installed by `deploy/install.sh`). NTP stays the preferred
//! source; the GPS only takes over without a network. Without chrony, or before it is configured, samples
//! are dropped: time sync is optional for the program (logged at `info` on change).

use std::io::ErrorKind;
use std::os::unix::net::UnixDatagram;
use std::time::{SystemTime, UNIX_EPOCH};

use chrono::{DateTime, Utc};

pub const SOCKET: &str = "/run/chrony.racecast.sock";
/// `SOCK_MAGIC` of chrony's `refclock_sock.c`.
const MAGIC: i32 = 0x534f_434b;
/// GNSS times before this are rejected: a receiver hit by a week-number rollover reports a date decades
/// in the past, which must never reach the system clock.
const MIN_PLAUSIBLE_S: i64 = 1_767_225_600; // 2026-01-01T00:00:00Z

// `struct sock_sample` below assumes 64-bit `time_t` and `suseconds_t` (aarch64 Linux).
const _: () = assert!(size_of::<usize>() == 8);

/// Encodes chrony's `struct sock_sample { struct timeval tv; double offset; int pulse; int leap; int _pad;
/// int magic; }`: `tv` = local time when the sentence was received, `offset` = GNSS time − `tv`.
fn sample(received: SystemTime, gps: DateTime<Utc>) -> Option<[u8; 40]> {
    if gps.timestamp() < MIN_PLAUSIBLE_S {
        return None;
    }
    let since = received.duration_since(UNIX_EPOCH).ok()?;
    let tv_sec = i64::try_from(since.as_secs()).ok()?;
    let tv_usec = i64::from(since.subsec_micros());
    let offset = (gps.timestamp() - tv_sec) as f64 + (i64::from(gps.timestamp_subsec_micros()) - tv_usec) as f64 / 1e6;
    let mut out = [0u8; 40];
    out[0..8].copy_from_slice(&tv_sec.to_ne_bytes());
    out[8..16].copy_from_slice(&tv_usec.to_ne_bytes());
    out[16..24].copy_from_slice(&offset.to_ne_bytes());
    // pulse = 0, leap = 0 (no leap second announced), _pad = 0: already zero.
    out[36..40].copy_from_slice(&MAGIC.to_ne_bytes());
    Some(out)
}

/// Sends GPS time samples to chrony.
#[derive(Default)]
pub struct Chrony {
    socket: Option<UnixDatagram>,
    /// Last delivery result, to log only the changes.
    reachable: Option<bool>,
}

impl Chrony {
    /// `received`: local time when the RMC sentence was read; `gps`: the time it carries.
    pub fn send(&mut self, received: SystemTime, gps: DateTime<Utc>) {
        let Some(bytes) = sample(received, gps) else { return };
        if self.socket.is_none() {
            // Non-blocking: if chrony stops reading, samples are dropped instead of stalling the GPS task.
            self.socket = UnixDatagram::unbound().and_then(|s| s.set_nonblocking(true).map(|()| s)).ok();
        }
        let Some(socket) = &self.socket else { return };
        let reachable = match socket.send_to(&bytes, SOCKET) {
            Ok(_) => true,
            Err(e) if e.kind() == ErrorKind::WouldBlock => return,
            Err(e) => {
                if self.reachable != Some(false) {
                    tracing::info!(socket = SOCKET, error = %e, "chrony unreachable: GPS time not shared");
                }
                false
            }
        };
        if reachable && self.reachable != Some(true) {
            tracing::info!(socket = SOCKET, "GPS time shared with chrony");
        }
        self.reachable = Some(reachable);
    }
}

#[cfg(test)]
mod tests {
    use std::time::Duration;

    use super::*;

    fn field<const N: usize>(bytes: &[u8; 40], at: usize) -> [u8; N] {
        bytes[at..at + N].try_into().unwrap()
    }

    #[test]
    fn sample_layout() {
        let received = UNIX_EPOCH + Duration::new(1_790_000_000, 250_000_000);
        let gps = DateTime::from_timestamp(1_790_000_000, 0).unwrap();
        let bytes = sample(received, gps).unwrap();
        assert_eq!(i64::from_ne_bytes(field(&bytes, 0)), 1_790_000_000);
        assert_eq!(i64::from_ne_bytes(field(&bytes, 8)), 250_000);
        // The sentence arrived 250 ms after the fix time it carries.
        assert!((f64::from_ne_bytes(field(&bytes, 16)) + 0.25).abs() < 1e-9);
        assert_eq!(bytes[24..36], [0; 12]);
        assert_eq!(i32::from_ne_bytes(field(&bytes, 36)), 0x534f_434b);
    }

    #[test]
    fn implausible_gnss_date_rejected() {
        // Week-number rollover: 1024 weeks (~19.6 years) in the past.
        let gps = DateTime::from_timestamp(1_790_000_000 - 1024 * 7 * 86_400, 0).unwrap();
        assert!(sample(UNIX_EPOCH + Duration::from_secs(1_790_000_000), gps).is_none());
    }
}
