//! Logs: JSON in `LOG_DIR` (one file per day, everything is kept) + compact text on stderr (journald
//! under systemd). The logger never panics and never blocks: file writes happen on a dedicated thread,
//! and lines are dropped if its buffer is full.
//!
//! stdout itself is sent to `/dev/null` ([`silence_stdout`]): vendor libraries print debug output there
//! (NVIDIA's `NvVideoEncoder::setBitrate` prints two pixel formats at every bitrate change of the LiveKit
//! encoder, several times per second on a real network), which would flood the journal. Their errors go
//! to stderr and are kept.

use std::path::Path;

use tracing_appender::non_blocking::WorkerGuard;
use tracing_appender::rolling::{RollingFileAppender, Rotation};
use tracing_subscriber::layer::SubscriberExt;
use tracing_subscriber::util::SubscriberInitExt;
use tracing_subscriber::{EnvFilter, fmt};

/// Must stay alive until the end of the program: flushes the log file buffer when dropped.
pub struct LogGuard {
    _file: Option<WorkerGuard>,
}

/// Initializes logging. If `log_dir` is unusable, logs go to stderr only and the error is returned so it
/// can be logged once the subscriber is in place.
pub fn init(log_dir: &Path, filter: &str) -> (LogGuard, Option<String>) {
    let filter = EnvFilter::try_new(filter).unwrap_or_else(|_| EnvFilter::new("info"));
    let (file_layer, guard, error) = match open_appender(log_dir) {
        Ok(appender) => {
            let (writer, guard) = tracing_appender::non_blocking(appender);
            let layer = fmt::layer().json().with_current_span(true).with_span_list(false).with_writer(writer);
            (Some(layer), Some(guard), None)
        }
        Err(e) => (None, None, Some(format!("file logging disabled ({}): {e}", log_dir.display()))),
    };
    let console_layer = fmt::layer().compact().with_target(false).with_writer(std::io::stderr);
    let init = tracing_subscriber::registry().with(filter).with(file_layer).with(console_layer).try_init();
    let error = match (error, init) {
        (e, Ok(())) => e,
        (e, Err(init_err)) => {
            Some(format!("{}subscriber already installed: {init_err}", e.map(|e| e + "; ").unwrap_or_default()))
        }
    };
    (LogGuard { _file: guard }, error)
}

fn open_appender(log_dir: &Path) -> Result<RollingFileAppender, String> {
    std::fs::create_dir_all(log_dir).map_err(|e| e.to_string())?;
    RollingFileAppender::builder()
        .rotation(Rotation::DAILY)
        .filename_prefix("racecast")
        .filename_suffix("log")
        .build(log_dir)
        .map_err(|e| e.to_string())
}

/// Redirects stdout to `/dev/null` (see the module documentation).
pub fn silence_stdout() -> Result<(), String> {
    let null = std::fs::OpenOptions::new().write(true).open("/dev/null").map_err(|e| e.to_string())?;
    rustix::stdio::dup2_stdout(&null).map_err(|e| e.to_string())
}

/// Every panic is logged (message, location, backtrace) before the supervisor restarts the task.
pub fn install_panic_hook() {
    std::panic::set_hook(Box::new(|info| {
        let message = info
            .payload()
            .downcast_ref::<&str>()
            .map(|s| s.to_string())
            .or_else(|| info.payload().downcast_ref::<String>().cloned())
            .unwrap_or_else(|| "(non-text payload)".into());
        let location = info.location().map(|l| l.to_string()).unwrap_or_default();
        let thread = std::thread::current().name().unwrap_or("?").to_string();
        let backtrace = std::backtrace::Backtrace::force_capture().to_string();
        tracing::error!(%message, %location, %thread, %backtrace, "panic");
    }));
}
