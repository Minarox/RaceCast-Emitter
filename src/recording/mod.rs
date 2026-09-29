//! Local recording: one supervised task per camera (`.mov` H.264 + timecode) and per microphone (BWF).
//! Recording runs while the recording gate is open: requested (command bus) and enough disk space.

pub mod audio;
mod bwf;
mod timecode;
pub mod video;

use std::io;
use std::path::{Path, PathBuf};
use std::time::Duration;

use chrono::{DateTime, Local};
use tokio::sync::watch;
use tokio_util::sync::CancellationToken;

/// A branch that stopped producing buffers for this long is considered stuck.
const STALL_TIMEOUT: Duration = Duration::from_secs(5);
/// Time allowed for the first buffer after the pipeline starts.
const START_TIMEOUT: Duration = Duration::from_secs(10);
/// Delay before retrying a recording that failed while the capture keeps running (disk error…).
const RECORD_RETRY: Duration = Duration::from_secs(5);

/// New file `RECORDINGS_DIR/<YYYY-MM-DD>/<name>_<YYYY-MM-DDTHH-MM-SS>.<ext>` (day directory created), with a
/// numeric suffix if a file already has that name.
pub fn new_file_path(dir: &Path, name: &str, start: &DateTime<Local>, ext: &str) -> io::Result<PathBuf> {
    let day = dir.join(start.format("%Y-%m-%d").to_string());
    std::fs::create_dir_all(&day)?;
    let stem = format!("{name}_{}", start.format("%Y-%m-%dT%H-%M-%S"));
    let mut path = day.join(format!("{stem}.{ext}"));
    let mut n = 1;
    while path.exists() {
        path = day.join(format!("{stem}-{n}.{ext}"));
        n += 1;
    }
    Ok(path)
}

/// `element path: error (debug)` of a GStreamer error message.
fn describe(msg: &gstreamer::Message) -> String {
    use gstreamer::prelude::*;
    let src = msg.src().map(|s| s.path_string().to_string()).unwrap_or_default();
    match msg.view() {
        gstreamer::MessageView::Error(e) => {
            format!("{src}: {} ({})", e.error(), e.debug().map(|d| d.to_string()).unwrap_or_default())
        }
        _ => src,
    }
}

/// Keeps the recording gate up to date: open when recording is requested and the disk has room.
pub async fn gate(
    mut requested: watch::Receiver<bool>,
    mut disk_ok: watch::Receiver<bool>,
    gate: watch::Sender<bool>,
    shutdown: CancellationToken,
) -> Result<(), String> {
    loop {
        let open = *requested.borrow_and_update() && *disk_ok.borrow_and_update();
        if gate.send_if_modified(|cur| std::mem::replace(cur, open) != open) {
            tracing::info!(open, "recording gate changed");
        }
        tokio::select! {
            r = requested.changed() => r.map_err(|_| "recording request channel closed")?,
            r = disk_ok.changed() => r.map_err(|_| "disk state channel closed")?,
            () = shutdown.cancelled() => return Ok(()),
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use chrono::TimeZone;

    #[test]
    fn file_paths() {
        let dir = std::env::temp_dir().join(format!("racecast-paths-{}", std::process::id()));
        let start = Local.with_ymd_and_hms(2026, 9, 28, 13, 32, 1).unwrap();
        let p1 = new_file_path(&dir, "cam-front", &start, "mov").unwrap();
        assert_eq!(p1, dir.join("2026-09-28/cam-front_2026-09-28T13-32-01.mov"));
        std::fs::write(&p1, b"").unwrap();
        let p2 = new_file_path(&dir, "cam-front", &start, "mov").unwrap();
        assert_eq!(p2, dir.join("2026-09-28/cam-front_2026-09-28T13-32-01-1.mov"));
        std::fs::remove_dir_all(&dir).unwrap();
    }
}
