//! RaceCast-Emitter: capture, local recording and LiveKit streaming from the race car.
//! See `docs/SPEC.md`.

mod capture;
mod commands;
mod config;
mod logging;
mod recording;
mod storage;
mod stream;
mod supervisor;
mod systemd;
mod telemetry;

use std::path::PathBuf;
use std::process::ExitCode;
use std::sync::Arc;
use std::time::Duration;

use gstreamer as gst;
use tokio::signal::unix::{SignalKind, signal};
use tokio::sync::{Mutex, mpsc, watch};
use tokio_util::sync::CancellationToken;

use crate::commands::{Command, CommandBus, Source};
use crate::config::{DevicesFile, DevicesStore, Settings};
use crate::supervisor::{Backoff, Restart, Supervisor};

const USAGE: &str = "\
Usage: racecast-emitter [--env-file <path>] [--check-config]

  --env-file <path>  .env file to load (default: ./.env); relative paths inside it are resolved
                     from its directory
  --check-config     validate .env and devices.yml, print the problems and exit
                     (exit code 0 if everything is valid)

Signals: SIGTERM/SIGINT clean shutdown, SIGHUP reloads devices.yml,
         SIGUSR1/SIGUSR2 start/stop local recording.";

/// Time given to tasks to finalize their files on shutdown (must stay below `TimeoutStopSec`).
const SHUTDOWN_TIMEOUT: Duration = Duration::from_secs(15);
/// Extra time given to the async runtime to wind down once the tasks are done or abandoned.
const RUNTIME_SHUTDOWN_TIMEOUT: Duration = Duration::from_secs(2);

struct Args {
    env_file: PathBuf,
    check_config: bool,
}

fn parse_args() -> Result<Args, String> {
    let mut args = Args { env_file: PathBuf::from(".env"), check_config: false };
    let mut it = std::env::args_os().skip(1);
    while let Some(arg) = it.next() {
        match arg.to_str() {
            Some("--env-file") => {
                args.env_file = it.next().map(PathBuf::from).ok_or("--env-file expects a path")?;
            }
            Some("--check-config") => args.check_config = true,
            Some("-h" | "--help") => return Err(String::new()),
            _ => return Err(format!("unknown argument: {}", arg.to_string_lossy())),
        }
    }
    Ok(args)
}

fn main() -> ExitCode {
    let args = match parse_args() {
        Ok(a) => a,
        Err(e) => {
            if !e.is_empty() {
                eprintln!("{e}\n");
            }
            eprintln!("{USAGE}");
            return if e.is_empty() { ExitCode::SUCCESS } else { ExitCode::from(2) };
        }
    };

    let mut warnings = Vec::new();
    let settings = Settings::load(&args.env_file, &mut warnings);
    if args.check_config {
        return check_config(&settings, &warnings);
    }

    let (_log_guard, log_error) = logging::init(&settings.log_dir, &settings.log_filter);
    logging::install_panic_hook();
    tracing::info!(version = env!("CARGO_PKG_VERSION"), pid = std::process::id(), "starting");
    if let Some(e) = log_error {
        tracing::error!("{e}");
    }
    for w in &warnings {
        tracing::warn!("{w}");
    }
    if let Err(e) = logging::silence_stdout() {
        tracing::warn!(error = %e, "cannot redirect stdout to /dev/null: vendor debug output stays visible");
    }
    settings.log_summary();

    let runtime = match tokio::runtime::Builder::new_multi_thread().enable_all().build() {
        Ok(rt) => rt,
        Err(e) => {
            // Nothing can run without a runtime: systemd will restart the program.
            tracing::error!(error = %e, "cannot create the async runtime");
            return ExitCode::FAILURE;
        }
    };
    runtime.block_on(run(settings));
    // Never hang on exit: tasks stuck in a library call (GStreamer, WebRTC) are abandoned after a delay.
    runtime.shutdown_timeout(RUNTIME_SHUTDOWN_TIMEOUT);
    tracing::info!("stopped");
    ExitCode::SUCCESS
}

/// Validates the configuration without starting anything (after editing `.env` or `devices.yml` by hand).
fn check_config(settings: &Settings, warnings: &[String]) -> ExitCode {
    let mut ok = true;
    println!("{}: {} warning(s)", settings.env_file.display(), warnings.len());
    for w in warnings {
        println!("  - {w}");
        ok = false;
    }
    let path = &settings.devices_file;
    match std::fs::read_to_string(path) {
        Err(e) if e.kind() == std::io::ErrorKind::NotFound => {
            println!("{}: missing (will be created empty on startup)", path.display());
        }
        Err(e) => {
            println!("{}: unreadable ({e})", path.display());
            ok = false;
        }
        Ok(text) => match DevicesFile::parse(path, &text) {
            Ok(f) => println!(
                "{}: valid ({} camera override(s), {} microphone override(s))",
                path.display(),
                f.cameras.len(),
                f.microphones.len()
            ),
            Err(e) => {
                println!("{e}");
                ok = false;
            }
        },
    }
    if ok { ExitCode::SUCCESS } else { ExitCode::FAILURE }
}

async fn run(settings: Settings) {
    let shutdown = CancellationToken::new();
    let supervisor = Supervisor::new(shutdown.clone());

    let settings = Arc::new(settings);
    let devices = Arc::new(DevicesStore::open(settings.devices_file.clone()));

    // Recording gate = requested (command bus, enabled from startup) && enough disk space.
    let (requested, _) = watch::channel(true);
    let room = storage::initial_room(&settings.recordings_dir, settings.disk_critical_gb);
    let (disk_ok, _) = watch::channel(room);
    let (record_gate, _) = watch::channel(room);
    tracing::info!("local recording requested from startup");

    // Configuration changes: the dispatcher validates them, the device manager applies and confirms them.
    let (changes_tx, changes_rx) = mpsc::channel(4);
    let (bus, dispatcher) = commands::channel(commands::State {
        recording: requested.clone(),
        devices: devices.clone(),
        changes: changes_tx,
        applying: std::sync::atomic::AtomicBool::new(false),
    });
    if devices.pending_at_startup() {
        bus.send(Command::ReloadDevices, Source::Startup);
    }
    supervisor.spawn("commands", Restart::Always, move |token| dispatcher.clone().run(token));
    {
        let (dir, critical, disk_ok) = (settings.recordings_dir.clone(), settings.disk_critical_gb, disk_ok.clone());
        supervisor.spawn("storage", Restart::Always, move |token| {
            storage::monitor(dir.clone(), critical, disk_ok.clone(), token)
        });
    }
    {
        let (requested, disk_ok, gate) = (requested.clone(), disk_ok.clone(), record_gate.clone());
        supervisor.spawn("record-gate", Restart::Always, move |token| {
            recording::gate(requested.subscribe(), disk_ok.subscribe(), gate.clone(), token)
        });
    }
    let (active_devices, _) = watch::channel(Vec::new());
    let snapshot = Arc::new(telemetry::Snapshot::default());
    let hub = settings.livekit.clone().map(|lk| start_streaming(&supervisor, lk, &snapshot, &record_gate));
    match gst::init() {
        Ok(()) => {
            let capture = CaptureContext {
                record_gate: record_gate.subscribe(),
                active: active_devices.clone(),
                stream: hub.clone(),
                changes: changes_rx,
            };
            start_capture(&supervisor, &settings, &devices, capture);
        }
        Err(e) => {
            // No device manager: configuration changes are committed without confirmation.
            drop(changes_rx);
            tracing::error!(error = %e, "GStreamer unavailable: capture and recording disabled");
        }
    }
    start_telemetry(&supervisor, &settings, &record_gate, &active_devices, &snapshot, hub.as_ref());
    if let Some(period) = systemd::watchdog_period() {
        tracing::info!(period_ms = period.as_millis() as u64, "systemd watchdog enabled");
        supervisor.spawn("systemd-watchdog", Restart::Always, move |token| systemd::watchdog(period, token));
    }

    systemd::ready();
    handle_signals(&bus).await;

    systemd::stopping();
    tracing::info!("shutdown requested, finalizing tasks");
    shutdown.cancel();
    let stuck = supervisor.wait(SHUTDOWN_TIMEOUT).await;
    if !stuck.is_empty() {
        tracing::warn!(timeout_s = SHUTDOWN_TIMEOUT.as_secs(), tasks = %stuck.join(", "), "some tasks did not stop in time");
    }
}

/// Starts the LiveKit connection and the room metadata publisher. Returns the hub where devices register
/// their tracks.
fn start_streaming(
    supervisor: &Supervisor,
    lk: config::LiveKitSettings,
    snapshot: &Arc<telemetry::Snapshot>,
    record_gate: &watch::Sender<bool>,
) -> Arc<stream::StreamHub> {
    let hub = stream::StreamHub::new();
    {
        let (hub, lk) = (hub.clone(), lk.clone());
        // Short maximum delay: streaming must come back quickly once the network is back.
        let backoff = Backoff::new(Duration::from_secs(1), Duration::from_secs(15), Duration::from_secs(60));
        supervisor.spawn_with_backoff("livekit", Restart::Always, backoff, move |token| {
            stream::room::run(hub.clone(), lk.clone(), token)
        });
    }
    let (hub2, snapshot, gate) = (hub.clone(), snapshot.clone(), record_gate.subscribe());
    supervisor.spawn("room-metadata", Restart::Always, move |token| {
        let sources =
            stream::metadata::Sources { snapshot: snapshot.clone(), hub: hub2.clone(), recording: gate.clone() };
        stream::metadata::run(sources, lk.clone(), token)
    });
    hub
}

struct CaptureContext {
    record_gate: watch::Receiver<bool>,
    active: watch::Sender<Vec<capture::ActiveDevice>>,
    stream: Option<Arc<stream::StreamHub>>,
    changes: mpsc::Receiver<capture::manager::ChangeRequest>,
}

/// Starts hot-plug detection and the device manager (one supervised task per camera/microphone).
fn start_capture(supervisor: &Supervisor, settings: &Arc<Settings>, devices: &Arc<DevicesStore>, ctx: CaptureContext) {
    let (tx, rx) = mpsc::channel(64);
    supervisor.spawn("udev", Restart::Always, move |token| {
        let tx = tx.clone();
        async move {
            tokio::task::spawn_blocking(move || capture::udev::watch(tx, token))
                .await
                .map_err(|e| e.to_string())?
                .map_err(|e| e.to_string())
        }
    });
    let manager = capture::manager::Manager {
        settings: settings.clone(),
        supervisor: supervisor.clone(),
        devices: devices.clone(),
        recording: ctx.record_gate,
        events: Arc::new(Mutex::new(rx)),
        changes: Arc::new(Mutex::new(ctx.changes)),
        active: ctx.active,
        stream: ctx.stream,
    };
    supervisor.spawn("devices", Restart::Always, move |token| manager.clone().run(token));
}

/// Starts the telemetry sources (one supervised task each, one CSV file each).
fn start_telemetry(
    supervisor: &Supervisor,
    settings: &Arc<Settings>,
    record_gate: &watch::Sender<bool>,
    active_devices: &watch::Sender<Vec<capture::ActiveDevice>>,
    snapshot: &Arc<telemetry::Snapshot>,
    hub: Option<&Arc<stream::StreamHub>>,
) {
    let t = &settings.telemetry;
    let out = telemetry::Output {
        dir: settings.recordings_dir.clone(),
        gate: record_gate.subscribe(),
        snapshot: snapshot.clone(),
    };
    {
        let (period, out) = (t.gps_interval, out.clone());
        supervisor.spawn("gps", Restart::Always, move |token| telemetry::gps::run(period, out.clone(), token));
    }
    {
        let (period, out) = (t.modem_interval, out.clone());
        supervisor.spawn("modem", Restart::Always, move |token| telemetry::modem::run(period, out.clone(), token));
    }
    {
        let (bus, address, period, out) = (t.ups_i2c_bus, t.ups_i2c_address, t.ups_interval, out.clone());
        supervisor
            .spawn("ups", Restart::Always, move |token| telemetry::ups::run(bus, address, period, out.clone(), token));
    }
    let (period, devices, livekit) = (t.system_interval, active_devices.subscribe(), hub.map(|h| h.connected()));
    supervisor.spawn("system", Restart::Always, move |token| {
        let sources = telemetry::system::Sources { devices: devices.clone(), livekit: livekit.clone() };
        telemetry::system::run(period, out.clone(), sources, token)
    });
}

/// Turns signals into commands until SIGTERM/SIGINT. A signal that cannot be listened to is logged and
/// ignored: the program keeps running.
async fn handle_signals(bus: &CommandBus) {
    let listen = |kind: SignalKind, name: &str| match signal(kind) {
        Ok(s) => Some(s),
        Err(e) => {
            tracing::error!(signal = name, error = %e, "cannot listen to signal");
            None
        }
    };
    let mut term = listen(SignalKind::terminate(), "SIGTERM");
    let mut int = listen(SignalKind::interrupt(), "SIGINT");
    let mut hup = listen(SignalKind::hangup(), "SIGHUP");
    let mut usr1 = listen(SignalKind::user_defined1(), "SIGUSR1");
    let mut usr2 = listen(SignalKind::user_defined2(), "SIGUSR2");

    async fn recv(s: &mut Option<tokio::signal::unix::Signal>) {
        match s {
            Some(s) => {
                s.recv().await;
            }
            None => std::future::pending().await,
        }
    }

    loop {
        tokio::select! {
            () = recv(&mut term) => { tracing::info!("SIGTERM received"); return }
            () = recv(&mut int) => { tracing::info!("SIGINT received"); return }
            () = recv(&mut hup) => bus.send(Command::ReloadDevices, Source::Signal),
            () = recv(&mut usr1) => bus.send(Command::SetRecording(true), Source::Signal),
            () = recv(&mut usr2) => bus.send(Command::SetRecording(false), Source::Signal),
        }
    }
}
