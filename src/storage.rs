//! Disk: atomic writes and free-space monitoring of the recordings directory.

use std::fs::{self, File};
use std::io::{self, Write};
use std::path::{Path, PathBuf};
use std::time::Duration;

use tokio::sync::watch;
use tokio_util::sync::CancellationToken;

const GB: u64 = 1_000_000_000;
/// Free space needed above the critical threshold before recording resumes (avoids flapping).
const RESUME_MARGIN: u64 = GB;
const CHECK_PERIOD: Duration = Duration::from_secs(5);

/// Replaces `path` with `data` without ever leaving a half-written file: write to a temporary file in the
/// same directory, `fsync`, atomic `rename`, then `fsync` the directory.
pub fn write_atomic(path: &Path, data: &[u8]) -> io::Result<()> {
    let dir = match path.parent() {
        Some(d) if !d.as_os_str().is_empty() => d,
        _ => Path::new("."),
    };
    fs::create_dir_all(dir)?;
    let mut tmp_name = path.file_name().unwrap_or_default().to_os_string();
    tmp_name.push(format!(".tmp-{}", std::process::id()));
    let tmp = dir.join(tmp_name);

    let result = (|| {
        let mut f = File::create(&tmp)?;
        f.write_all(data)?;
        f.sync_all()?;
        fs::rename(&tmp, path)?;
        File::open(dir)?.sync_all()
    })();
    if result.is_err() {
        let _ = fs::remove_file(&tmp);
    }
    result
}

/// Free space available to the program in `dir` (created if missing).
pub fn free_bytes(dir: &Path) -> io::Result<u64> {
    fs::create_dir_all(dir)?;
    let st = rustix::fs::statvfs(dir)?;
    Ok(st.f_bavail.saturating_mul(st.f_frsize))
}

/// Whether recording may run, with hysteresis: stops below `critical`, resumes above `critical` + margin.
pub fn has_room(free: u64, critical: u64, currently_ok: bool) -> bool {
    if currently_ok { free >= critical } else { free >= critical.saturating_add(RESUME_MARGIN) }
}

/// Initial state, checked before any recording starts.
pub fn initial_room(dir: &Path, critical_gb: u64) -> bool {
    match free_bytes(dir) {
        Ok(free) => {
            let ok = has_room(free, critical_gb.saturating_mul(GB), true);
            if ok {
                tracing::info!(free_gb = free / GB, critical_gb, dir = %dir.display(), "recording disk space");
            } else {
                tracing::error!(free_gb = free / GB, critical_gb, dir = %dir.display(), "disk space critical: local recording disabled");
            }
            ok
        }
        Err(e) => {
            tracing::error!(dir = %dir.display(), error = %e, "recordings directory unusable");
            false
        }
    }
}

/// Watches the free space of the recordings directory and updates `disk_ok`. Local recording stops below
/// `DISK_CRITICAL_GB` (files finalized cleanly); LiveKit streaming is not affected.
pub async fn monitor(
    dir: PathBuf,
    critical_gb: u64,
    disk_ok: watch::Sender<bool>,
    token: CancellationToken,
) -> Result<(), String> {
    let critical = critical_gb.saturating_mul(GB);
    let mut tick = tokio::time::interval(CHECK_PERIOD);
    loop {
        tokio::select! {
            _ = tick.tick() => {}
            () = token.cancelled() => return Ok(()),
        }
        let current = *disk_ok.borrow();
        let ok = match free_bytes(&dir) {
            Ok(free) => {
                let ok = has_room(free, critical, current);
                if current && !ok {
                    tracing::error!(free_gb = free / GB, critical_gb, "disk space critical: local recording stopped");
                } else if !current && ok {
                    tracing::info!(
                        free_gb = free / GB,
                        critical_gb,
                        "disk space available again: local recording resumed"
                    );
                }
                ok
            }
            Err(e) => {
                if current {
                    tracing::error!(dir = %dir.display(), error = %e, "recordings directory unusable: local recording stopped");
                }
                false
            }
        };
        disk_ok.send_if_modified(|cur| std::mem::replace(cur, ok) != ok);
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn room_with_hysteresis() {
        let critical = 5 * GB;
        assert!(has_room(6 * GB, critical, true));
        assert!(!has_room(4 * GB, critical, true));
        // Stopped: resumes only with the margin.
        assert!(!has_room(5 * GB + GB / 2, critical, false));
        assert!(has_room(6 * GB, critical, false));
    }

    #[test]
    fn free_space_of_temp_dir() {
        assert!(free_bytes(&std::env::temp_dir()).unwrap() > 0);
    }

    #[test]
    fn replaces_content_without_leftovers() {
        let dir = std::env::temp_dir().join(format!("racecast-test-{}", std::process::id()));
        let path = dir.join("sub").join("f.yml");
        write_atomic(&path, b"one").unwrap();
        write_atomic(&path, b"two").unwrap();
        assert_eq!(fs::read(&path).unwrap(), b"two");
        let entries: Vec<_> = fs::read_dir(path.parent().unwrap()).unwrap().collect();
        assert_eq!(entries.len(), 1);
        fs::remove_dir_all(&dir).unwrap();
    }
}
