//! Internal command bus: the single entry point for every action (Unix signals today, recording button
//! and LiveKit RPC later). Each command can get a reply, for sources that expect an acknowledgement.

use std::fmt;
use std::sync::Arc;
use std::sync::atomic::{AtomicBool, Ordering};
use std::time::Duration;

use tokio::sync::{Mutex, mpsc, oneshot, watch};
use tokio_util::sync::CancellationToken;

use crate::capture::manager::{CONFIRM_TIMEOUT, ChangeRequest};
use crate::config::DevicesStore;

/// Upper bound for a configuration change (device stops and restarts included); the manager decides well
/// before, this only guards against a stuck manager.
const CHANGE_TIMEOUT: Duration = Duration::from_secs(CONFIRM_TIMEOUT.as_secs() * 4);

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum Command {
    /// Starts (`true`) or stops (`false`) local recording. Streaming is not affected.
    SetRecording(bool),
    /// Re-reads `devices.yml` (after a manual edit) and applies the new version if it is valid; it is kept
    /// only if the devices it changes confirm it, otherwise everything goes back (SPEC §8).
    ReloadDevices,
}

/// Where a command came from, for the logs.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum Source {
    Signal,
    /// Issued by the program itself at startup.
    Startup,
}

impl fmt::Display for Source {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        f.write_str(match self {
            Self::Signal => "signal",
            Self::Startup => "startup",
        })
    }
}

pub type Reply = Result<String, String>;

struct Request {
    command: Command,
    source: Source,
    reply: Option<oneshot::Sender<Reply>>,
}

/// Sending side, cloneable and shared between command sources.
#[derive(Clone)]
pub struct CommandBus {
    tx: mpsc::Sender<Request>,
}

impl CommandBus {
    /// Sends a command without waiting for the reply. Never blocks: if the bus is full, the command is
    /// dropped with a warning.
    pub fn send(&self, command: Command, source: Source) {
        let request = Request { command, source, reply: None };
        if let Err(e) = self.tx.try_send(request) {
            tracing::warn!(?command, %source, error = %e, "command dropped");
        }
    }

    /// Sends a command and waits for its result (future LiveKit RPC).
    #[allow(dead_code)] // wired up with the admin page (LiveKit RPC)
    pub async fn request(&self, command: Command, source: Source) -> Reply {
        let (reply, rx) = oneshot::channel();
        self.tx
            .send(Request { command, source, reply: Some(reply) })
            .await
            .map_err(|_| "command bus stopped".to_string())?;
        rx.await.map_err(|_| "command got no reply".to_string())?
    }
}

/// State driven by commands and observed by the relevant tasks.
pub struct State {
    /// Requested local recording state (enabled from startup).
    pub recording: watch::Sender<bool>,
    pub devices: Arc<DevicesStore>,
    /// Configuration changes, applied and confirmed by the device manager.
    pub changes: mpsc::Sender<ChangeRequest>,
    /// A configuration change is in progress (one at a time).
    pub applying: AtomicBool,
}

/// Receiving side. Shared behind a mutex so that an instance restarted by the supervisor picks up the
/// queue where the previous one stopped.
#[derive(Clone)]
pub struct Dispatcher {
    rx: Arc<Mutex<mpsc::Receiver<Request>>>,
    state: Arc<State>,
}

pub fn channel(state: State) -> (CommandBus, Dispatcher) {
    let (tx, rx) = mpsc::channel(64);
    (CommandBus { tx }, Dispatcher { rx: Arc::new(Mutex::new(rx)), state: Arc::new(state) })
}

impl Dispatcher {
    /// Processes commands until `shutdown` is cancelled.
    pub async fn run(self, shutdown: CancellationToken) -> Result<(), String> {
        let mut rx = self.rx.lock().await;
        loop {
            let request = tokio::select! {
                r = rx.recv() => r,
                () = shutdown.cancelled() => return Ok(()),
            };
            let Some(Request { command, source, reply }) = request else {
                return Err("all command senders are gone".into());
            };
            match command {
                Command::SetRecording(on) => finish(command, source, reply, self.set_recording(on)),
                // A change takes several seconds to confirm: run it aside so that other commands (recording
                // button) are not held back.
                Command::ReloadDevices => {
                    let state = self.state.clone();
                    tokio::spawn(async move { finish(command, source, reply, reload_devices(&state).await) });
                }
            }
        }
    }

    fn set_recording(&self, on: bool) -> Reply {
        let changed = self.state.recording.send_if_modified(|cur| std::mem::replace(cur, on) != on);
        let what = if on { "started" } else { "stopped" };
        Ok(if changed { format!("recording {what}") } else { format!("recording already {what}") })
    }
}

fn finish(command: Command, source: Source, reply: Option<oneshot::Sender<Reply>>, result: Reply) {
    match &result {
        Ok(msg) => tracing::info!(?command, %source, "{msg}"),
        Err(msg) => tracing::error!(?command, %source, "{msg}"),
    }
    if let Some(reply) = reply {
        let _ = reply.send(result);
    }
}

/// Validates `devices.yml`, has the device manager apply and confirm it, then commits it, or restores the
/// confirmed version on disk if the change was rolled back.
async fn reload_devices(state: &State) -> Reply {
    struct Busy<'a>(&'a AtomicBool);
    impl Drop for Busy<'_> {
        fn drop(&mut self) {
            self.0.store(false, Ordering::Release);
        }
    }
    if state.applying.swap(true, Ordering::AcqRel) {
        return Err("a devices.yml change is already being applied, try again once it is done".into());
    }
    let _busy = Busy(&state.applying);
    apply_devices(state).await
}

async fn apply_devices(state: &State) -> Reply {
    let store = state.devices.clone();
    // Disk access: keep it off the async worker threads.
    let candidate = match tokio::task::spawn_blocking(move || store.read()).await {
        Ok(Ok(Some(candidate))) => candidate,
        Ok(Ok(None)) => return Ok("devices.yml re-read, no change".into()),
        Ok(Err(e)) => return Err(format!("{e}; the previous configuration stays active")),
        Err(e) => return Err(format!("devices.yml reload interrupted: {e}")),
    };
    let (reply, outcome) = oneshot::channel();
    let outcome = match state.changes.send(ChangeRequest { file: candidate.file.clone(), reply }).await {
        // No device manager (GStreamer unavailable): nothing runs, nothing to confirm.
        Err(_) => Ok("capture not running, nothing to confirm".to_string()),
        Ok(()) => match tokio::time::timeout(CHANGE_TIMEOUT, outcome).await {
            Ok(Ok(outcome)) => outcome,
            Ok(Err(_)) => Err("interrupted (device manager restarted or shutting down)".into()),
            Err(_) => Err(format!("no outcome within {} s", CHANGE_TIMEOUT.as_secs())),
        },
    };
    let store = state.devices.clone();
    match outcome {
        Ok(message) => {
            let _ = tokio::task::spawn_blocking(move || store.commit(candidate)).await;
            Ok(format!("devices.yml applied: {message}"))
        }
        Err(reason) => {
            let restored = tokio::task::spawn_blocking(move || store.reject(&candidate))
                .await
                .map_err(|e| e.to_string())
                .and_then(|r| r);
            let restored = restored.unwrap_or_else(|e| format!("could not restore the file: {e}"));
            Err(format!("devices.yml rolled back ({reason}); previous configuration active; {restored}"))
        }
    }
}
