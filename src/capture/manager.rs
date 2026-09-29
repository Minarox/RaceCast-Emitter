//! Device manager: turns udev events into one supervised task per camera/microphone and applies the
//! `devices.yml` overrides.
//!
//! A configuration change is a transaction (SPEC §8): only the devices whose effective configuration
//! changes are restarted; the change is confirmed once each of them has delivered frames for
//! [`CONFIRM_WINDOW`] without any failure. The first failure, or no steady frames within
//! [`CONFIRM_TIMEOUT`], rolls every device of the change back to its previous configuration (the other
//! devices are never touched). All or nothing: `devices.yml` always describes what runs, and cross-device
//! rules (unique names, one main camera) cannot be broken by a partial result.

use std::collections::HashMap;
use std::sync::Arc;
use std::time::{Duration, Instant};

use tokio::sync::{Mutex, mpsc, oneshot, watch};
use tokio::task::JoinHandle;
use tokio_util::sync::CancellationToken;

use super::health::{Health, Verdict};
use super::{ActiveDevice, DeviceEvent, DeviceInfo, DeviceKind, naming, udev};
use crate::config::{CameraConfig, DevicesFile, DevicesStore, MicConfig, Settings};
use crate::recording::{audio, video};
use crate::stream::StreamHub;
use crate::supervisor::{Restart, Supervisor};

/// Time allowed for a device task to finalize its files before a restart with a new configuration.
const STOP_TIMEOUT: Duration = Duration::from_secs(20);
/// Steady frames required from every restarted device before a change is confirmed.
pub const CONFIRM_WINDOW: Duration = Duration::from_secs(5);
/// A restarted device that is not steady by then rolls the change back (capture start timeout, USB
/// bandwidth fallbacks, plus the window).
pub const CONFIRM_TIMEOUT: Duration = Duration::from_secs(30);
const CHECK_PERIOD: Duration = Duration::from_millis(500);

/// A new `devices.yml` to apply; the reply tells whether it was confirmed (`Ok`) or rolled back (`Err`).
pub struct ChangeRequest {
    pub file: Arc<DevicesFile>,
    pub reply: oneshot::Sender<Result<String, String>>,
}

#[derive(Clone)]
pub struct Manager {
    pub settings: Arc<Settings>,
    pub supervisor: Supervisor,
    pub devices: Arc<DevicesStore>,
    /// Recording gate, handed to every device task.
    pub recording: watch::Receiver<bool>,
    /// udev events. Shared so that a restarted manager keeps the same queue.
    pub events: Arc<Mutex<mpsc::Receiver<DeviceEvent>>>,
    /// Configuration changes to apply (shared like `events`).
    pub changes: Arc<Mutex<mpsc::Receiver<ChangeRequest>>>,
    /// Devices currently handled, kept up to date for the other modules.
    pub active: watch::Sender<Vec<ActiveDevice>>,
    /// LiveKit streaming, if configured.
    pub stream: Option<Arc<StreamHub>>,
}

#[derive(Debug, Clone, PartialEq)]
enum DeviceConfig {
    Camera(CameraConfig),
    Microphone(MicConfig),
}

impl DeviceConfig {
    fn name(&self) -> &str {
        match self {
            Self::Camera(c) => &c.name,
            Self::Microphone(m) => &m.name,
        }
    }

    fn active(&self) -> ActiveDevice {
        match self {
            Self::Camera(c) => ActiveDevice { kind: DeviceKind::Camera, name: c.name.clone(), main: c.main },
            Self::Microphone(m) => ActiveDevice { kind: DeviceKind::Microphone, name: m.name.clone(), main: false },
        }
    }
}

struct Entry {
    info: DeviceInfo,
    config: DeviceConfig,
    /// Name chosen automatically (kept across reconfigurations while no override names the device).
    auto_name: Option<String>,
    health: Health,
    scope: CancellationToken,
    handle: JoinHandle<()>,
}

/// A configuration change waiting for its devices to confirm it.
struct Transaction {
    previous: Arc<DevicesFile>,
    next: Arc<DevicesFile>,
    /// Devices restarted by the change (syspath), with the configuration to go back to.
    changed: HashMap<String, (DeviceConfig, Option<String>)>,
    deadline: Instant,
    reply: oneshot::Sender<Result<String, String>>,
}

impl Manager {
    /// Runs until `token` is cancelled. Device tasks are scoped to this instance: if it stops (panic
    /// included), they are cancelled and the restarted manager re-enumerates the devices.
    /// A change in progress when this instance stops is abandoned (its reply is dropped, so it is not
    /// committed); the restarted manager starts again from the confirmed configuration.
    pub async fn run(self, token: CancellationToken) -> Result<(), String> {
        let _cancel_devices_on_exit = token.clone().drop_guard();
        let mut events = self.events.lock().await;
        let mut changes = self.changes.lock().await;
        let mut applied = self.devices.current();
        let mut pending: Option<Transaction> = None;
        let mut check = tokio::time::interval(CHECK_PERIOD);
        let mut entries: HashMap<String, Entry> = HashMap::new();
        self.resync(&mut entries, &applied, None, &token).await;
        loop {
            self.publish(&entries);
            tokio::select! {
                // Cancellation first: at shutdown the event and change channels close right after it, which
                // must not be reported as a failure.
                biased;
                () = token.cancelled() => {
                    self.active.send_replace(Vec::new());
                    return Ok(());
                }
                event = events.recv() => match event {
                    Some(DeviceEvent::Added(info)) => self.add(&mut entries, info, &applied, pending.as_mut(), &token),
                    Some(DeviceEvent::Removed { syspath }) => remove(&mut entries, &syspath),
                    Some(DeviceEvent::Resync) => self.resync(&mut entries, &applied, pending.as_mut(), &token).await,
                    None => return Err("udev event channel closed".into()),
                },
                request = changes.recv() => {
                    let Some(request) = request else { return Err("configuration change channel closed".into()) };
                    if pending.is_some() {
                        let _ = request.reply.send(Err("a configuration change is already being confirmed".into()));
                        continue;
                    }
                    pending = self.begin(&mut entries, &mut applied, request, &token).await;
                }
                _ = check.tick(), if pending.is_some() => {
                    let Some(outcome) = pending.as_ref().and_then(|t| evaluate(&entries, t)) else { continue };
                    let Some(transaction) = pending.take() else { continue };
                    match outcome {
                        Ok(message) => {
                            tracing::info!("configuration change confirmed: {message}");
                            applied = transaction.next.clone();
                            let _ = transaction.reply.send(Ok(message));
                        }
                        Err(reason) => {
                            tracing::error!(%reason, "configuration change failed, rolling back");
                            self.rollback(&mut entries, &transaction, &token).await;
                            let _ = transaction.reply.send(Err(reason));
                        }
                    }
                }
            }
        }
    }

    fn publish(&self, entries: &HashMap<String, Entry>) {
        let mut active: Vec<ActiveDevice> = entries.values().map(|e| e.config.active()).collect();
        active.sort_by(|a, b| a.name.cmp(&b.name));
        self.active.send_if_modified(|cur| {
            let changed = *cur != active;
            if changed {
                *cur = active;
            }
            changed
        });
    }

    /// Reconciles the running tasks with the devices actually present.
    async fn resync(
        &self,
        entries: &mut HashMap<String, Entry>,
        applied: &Arc<DevicesFile>,
        mut pending: Option<&mut Transaction>,
        token: &CancellationToken,
    ) {
        let present = match tokio::task::spawn_blocking(udev::enumerate).await {
            Ok(Ok(present)) => present,
            Ok(Err(e)) => return tracing::error!(error = %e, "cannot enumerate devices"),
            Err(e) => return tracing::error!(error = %e, "device enumeration interrupted"),
        };
        let gone: Vec<String> = entries.keys().filter(|k| !present.iter().any(|d| &d.syspath == *k)).cloned().collect();
        for syspath in gone {
            remove(entries, &syspath);
        }
        for info in present {
            self.add(entries, info, applied, pending.as_deref_mut(), token);
        }
    }

    /// Starts a new device. During a change, it runs with the new configuration and joins the change (it
    /// must confirm it too, and goes back with the others on a rollback).
    fn add(
        &self,
        entries: &mut HashMap<String, Entry>,
        info: DeviceInfo,
        applied: &Arc<DevicesFile>,
        pending: Option<&mut Transaction>,
        token: &CancellationToken,
    ) {
        if entries.contains_key(&info.syspath) {
            return;
        }
        let file = pending.as_ref().map_or(applied, |t| &t.next).clone();
        let (config, auto_name) = self.resolve(&file, &info, entries, None);
        if let Some(t) = pending {
            let previous = self.resolve(&t.previous, &info, entries, None);
            if previous.0 != config {
                t.changed.insert(info.syspath.clone(), previous);
            }
        }
        tracing::info!(
            kind = %info.kind,
            name = config.name(),
            devnode = %info.devnode,
            usb_path = info.usb_port.as_deref().unwrap_or("-"),
            by_id = info.by_id.as_deref().unwrap_or("-"),
            model = %info.model,
            configured = auto_name.is_none(),
            "device detected"
        );
        let entry = self.spawn(info, config, auto_name, token);
        entries.insert(entry.info.syspath.clone(), entry);
    }

    /// Starts a configuration change: restarts the devices whose effective configuration changes. Returns
    /// the transaction to confirm, or `None` (already answered) when no device is affected.
    async fn begin(
        &self,
        entries: &mut HashMap<String, Entry>,
        applied: &mut Arc<DevicesFile>,
        request: ChangeRequest,
        token: &CancellationToken,
    ) -> Option<Transaction> {
        let next = request.file;
        let mut changed = HashMap::new();
        let keys: Vec<String> = entries.keys().cloned().collect();
        for key in keys {
            let Some(entry) = entries.remove(&key) else { continue };
            let (config, auto_name) = self.resolve(&next, &entry.info, entries, entry.auto_name.as_deref());
            if config == entry.config {
                entries.insert(key, entry);
                continue;
            }
            tracing::info!(
                old = entry.config.name(),
                new = config.name(),
                "device configuration changed, restarting it"
            );
            let previous = (entry.config.clone(), entry.auto_name.clone());
            let info = stop(entry).await;
            entries.insert(key.clone(), self.spawn(info, config, auto_name, token));
            changed.insert(key, previous);
        }
        if changed.is_empty() {
            *applied = next;
            let _ = request.reply.send(Ok("no device affected".into()));
            return None;
        }
        let names: Vec<&str> = changed.keys().filter_map(|k| entries.get(k)).map(|e| e.config.name()).collect();
        tracing::info!(
            devices = %names.join(", "),
            window_s = CONFIRM_WINDOW.as_secs(),
            timeout_s = CONFIRM_TIMEOUT.as_secs(),
            "waiting for the restarted devices to confirm the new configuration"
        );
        Some(Transaction {
            previous: applied.clone(),
            next,
            changed,
            deadline: Instant::now() + CONFIRM_TIMEOUT,
            reply: request.reply,
        })
    }

    /// Puts every device of a failed change back on its previous configuration.
    async fn rollback(
        &self,
        entries: &mut HashMap<String, Entry>,
        transaction: &Transaction,
        token: &CancellationToken,
    ) {
        for (key, (config, auto_name)) in &transaction.changed {
            // Unplugged meanwhile: it will start with the confirmed configuration when plugged back.
            let Some(entry) = entries.remove(key) else { continue };
            tracing::warn!(name = config.name(), "restoring the previous device configuration");
            let info = stop(entry).await;
            entries.insert(key.clone(), self.spawn(info, config.clone(), auto_name.clone(), token));
        }
    }

    /// Effective configuration of a device, and its automatic name if no override names it.
    fn resolve(
        &self,
        file: &DevicesFile,
        info: &DeviceInfo,
        others: &HashMap<String, Entry>,
        current_auto: Option<&str>,
    ) -> (DeviceConfig, Option<String>) {
        let (port, id) = (info.usb_port.as_deref(), info.by_id.as_deref());
        let configured_name = match info.kind {
            DeviceKind::Camera => file.camera(port, id).and_then(|o| o.name.clone()),
            DeviceKind::Microphone => file.microphone(port, id).and_then(|o| o.name.clone()),
        };
        let auto_name = configured_name.is_none().then(|| {
            current_auto.map(str::to_string).unwrap_or_else(|| {
                let base = naming::auto_name(info.kind, &info.model);
                naming::unique(&base, |n| {
                    others.values().any(|e| e.config.name() == n) || file.reserved_names().any(|r| r == n)
                })
            })
        });
        let name = configured_name.or_else(|| auto_name.clone()).unwrap_or_default();
        let config = match info.kind {
            DeviceKind::Camera => {
                DeviceConfig::Camera(CameraConfig::resolve(&self.settings, name, file.camera(port, id)))
            }
            DeviceKind::Microphone => {
                DeviceConfig::Microphone(MicConfig::resolve(&self.settings, name, file.microphone(port, id)))
            }
        };
        (config, auto_name)
    }

    fn spawn(
        &self,
        info: DeviceInfo,
        config: DeviceConfig,
        auto_name: Option<String>,
        token: &CancellationToken,
    ) -> Entry {
        let scope = token.child_token();
        let health = Health::default();
        let dir = self.settings.recordings_dir.clone();
        let handle = match &config {
            DeviceConfig::Camera(c) => {
                let s = &c.stream;
                tracing::info!(
                    name = %c.name,
                    rotation = u16::from(c.rotation),
                    main = c.main,
                    bitrate_kbps = c.video_bitrate_kbps,
                    stream_enabled = s.enabled,
                    stream = %format!("{}@{} {} kbps", s.resolution, s.fps, s.bitrate_kbps),
                    "camera configuration"
                );
                let cam = Arc::new(video::Camera {
                    info: info.clone(),
                    config: c.clone(),
                    recordings_dir: dir,
                    recording: self.recording.clone(),
                    stream: self.stream.clone(),
                    health: health.clone(),
                });
                self.supervisor.spawn_scoped(format!("camera:{}", c.name), Restart::OnError, scope.clone(), move |t| {
                    let cam = cam.clone();
                    async move {
                        let result = video::run(cam.clone(), t).await;
                        if let Err(e) = &result {
                            cam.health.failed(&e.to_string());
                        }
                        result
                    }
                })
            }
            DeviceConfig::Microphone(m) => {
                tracing::info!(
                    name = %m.name,
                    channels = ?m.channels,
                    bit_depth = ?m.bit_depth,
                    stream_enabled = m.stream.enabled,
                    stream_bitrate_kbps = m.stream.bitrate_kbps,
                    "microphone configuration"
                );
                let mic = Arc::new(audio::Microphone {
                    info: info.clone(),
                    config: m.clone(),
                    recordings_dir: dir,
                    recording: self.recording.clone(),
                    stream: self.stream.clone(),
                    health: health.clone(),
                });
                self.supervisor.spawn_scoped(format!("mic:{}", m.name), Restart::OnError, scope.clone(), move |t| {
                    let mic = mic.clone();
                    async move {
                        let result = audio::run(mic.clone(), t).await;
                        if let Err(e) = &result {
                            mic.health.failed(&e.to_string());
                        }
                        result
                    }
                })
            }
        };
        Entry { info, config, auto_name, health, scope, handle }
    }
}

/// Stops a device task, giving it time to finalize its files.
async fn stop(entry: Entry) -> DeviceInfo {
    entry.scope.cancel();
    if tokio::time::timeout(STOP_TIMEOUT, entry.handle).await.is_err() {
        tracing::warn!(name = entry.config.name(), "device task slow to stop, restarting anyway");
    }
    entry.info
}

/// Outcome of a change once it is decided: every restarted device steady (`Ok`), or the first failure /
/// the timeout (`Err`). `None` while waiting.
fn evaluate(entries: &HashMap<String, Entry>, transaction: &Transaction) -> Option<Result<String, String>> {
    let mut waiting = Vec::new();
    let mut confirmed = Vec::new();
    for key in transaction.changed.keys() {
        // Unplugged meanwhile: cannot be judged, it will start with the result when plugged back.
        let Some(entry) = entries.get(key) else { continue };
        match entry.health.verdict(CONFIRM_WINDOW) {
            Verdict::Failed(error) => return Some(Err(format!("{}: {error}", entry.config.name()))),
            Verdict::Pending => waiting.push(entry.config.name()),
            Verdict::Healthy => confirmed.push(entry.config.name()),
        }
    }
    if waiting.is_empty() {
        if confirmed.is_empty() {
            return Some(Ok("the restarted devices were unplugged, nothing left to check".into()));
        }
        confirmed.sort_unstable();
        return Some(Ok(format!("{} steady for {} s", confirmed.join(", "), CONFIRM_WINDOW.as_secs())));
    }
    if Instant::now() >= transaction.deadline {
        return Some(Err(format!("no steady frames within {} s: {}", CONFIRM_TIMEOUT.as_secs(), waiting.join(", "))));
    }
    None
}

fn remove(entries: &mut HashMap<String, Entry>, syspath: &str) {
    if let Some(entry) = entries.remove(syspath) {
        tracing::info!(kind = %entry.info.kind, name = entry.config.name(), "device removed");
        entry.scope.cancel();
    }
}
