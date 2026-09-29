//! Microphone task. The capture pipeline runs as long as the microphone is plugged in:
//!
//! ```text
//! alsasrc (hw, native format) → tee → queue → appsink → BWF writer
//! ```
//!
//! The appsink always receives the samples; the writer opens a new BWF file when the recording gate opens
//! and finalizes it when the gate closes (no pipeline change needed, unlike video). The same samples feed
//! the LiveKit track (Opus) while it is published, whatever the recording state.

use std::path::PathBuf;
use std::sync::atomic::{AtomicU64, Ordering};
use std::sync::{Arc, Mutex};
use std::time::{Duration, Instant};

use futures_util::StreamExt;
use gstreamer as gst;
use gstreamer::prelude::*;
use gstreamer_app as gst_app;
use livekit::webrtc::audio_source::AudioSourceOptions;
use livekit::webrtc::audio_source::native::NativeAudioSource;
use tokio::sync::watch;
use tokio_util::sync::CancellationToken;

use super::bwf::{BwfSpec, BwfWriter};
use super::{RECORD_RETRY, STALL_TIMEOUT, START_TIMEOUT, describe, new_file_path, timecode};
use crate::capture::DeviceInfo;
use crate::capture::health::Health;
use crate::capture::modes::{self, AudioFormat};
use crate::config::MicConfig;
use crate::stream::audio::AudioTap;
use crate::stream::{Registration, StreamHub};

/// Timecode rate written in the iXML chunk (the BWF sync itself relies on `TimeReference`).
const TIMECODE_RATE: u32 = 30;

pub struct Microphone {
    pub info: DeviceInfo,
    pub config: MicConfig,
    pub recordings_dir: PathBuf,
    /// Recording gate (requested and enough disk space).
    pub recording: watch::Receiver<bool>,
    /// LiveKit streaming, if configured.
    pub stream: Option<Arc<StreamHub>>,
    /// Start, samples and failures, for the confirmation of configuration changes.
    pub health: Health,
}

#[derive(Debug, thiserror::Error)]
pub enum MicError {
    #[error("cannot read the microphone formats: {0}")]
    Probe(String),
    #[error("cannot build the pipeline: {0}")]
    Build(String),
    #[error("pipeline error: {0}")]
    Pipeline(String),
    #[error("no audio received for {0:?}")]
    Stalled(Duration),
}

/// Runs the microphone until `token` is cancelled (unplugged, configuration change, shutdown).
pub async fn run(mic: Arc<Microphone>, token: CancellationToken) -> Result<(), MicError> {
    let card = mic.info.alsa_card.clone().ok_or_else(|| MicError::Probe("no ALSA card".into()))?;
    let device = format!("hw:CARD={card},DEV=0");
    let probe_device = device.clone();
    let caps = tokio::task::spawn_blocking(move || probe_caps(&probe_device))
        .await
        .map_err(|e| MicError::Probe(e.to_string()))??;
    let c = &mic.config;
    let candidates = modes::audio_candidates(&modes::audio_caps(&caps), c.sample_rate, c.bit_depth, c.channels);
    if candidates.is_empty() {
        tracing::error!(%caps, "no recordable PCM format (S16LE/S24LE/S32LE): microphone ignored");
        return Ok(());
    }
    let mut last_error = None;
    for format in candidates {
        match Capture::start(&mic, &device, format).await {
            Ok(capture) => {
                let honored = c.bit_depth.is_none_or(|b| b == u32::from(format.format.bits()))
                    && c.channels.is_none_or(|n| n == format.channels);
                if !honored {
                    tracing::warn!(
                        requested_bits = ?c.bit_depth,
                        requested_channels = ?c.channels,
                        %format,
                        "requested format refused by the microphone, using the closest one"
                    );
                }
                return capture.run(&mic, token).await;
            }
            Err(e) => {
                tracing::info!(%format, error = %e, "format refused by the microphone, trying the next one");
                last_error = Some(e);
            }
        }
        if token.is_cancelled() {
            return Ok(());
        }
    }
    Err(last_error.unwrap_or(MicError::Probe("no format could be opened".into())))
}

fn probe_caps(device: &str) -> Result<gst::Caps, MicError> {
    let err = |e: &dyn std::fmt::Display| MicError::Probe(e.to_string());
    let src = gst::ElementFactory::make("alsasrc").property("device", device).build().map_err(|e| err(&e))?;
    src.set_state(gst::State::Ready).map_err(|e| err(&e))?;
    let caps = src.static_pad("src").map(|p| p.query_caps(None));
    let _ = src.set_state(gst::State::Null);
    caps.ok_or_else(|| err(&"alsasrc has no source pad"))
}

struct Capture {
    pipeline: gst::Pipeline,
    recorder: Arc<Mutex<Recorder>>,
    /// Keeps the LiveKit track registered while the capture runs.
    _registration: Option<Registration>,
    /// Milliseconds since `epoch` of the last samples received (0 = none yet).
    last_buffer: Arc<AtomicU64>,
    epoch: Instant,
}

impl Capture {
    /// Starts the capture in `format` and waits for the first samples.
    async fn start(mic: &Microphone, device: &str, format: AudioFormat) -> Result<Self, MicError> {
        let build = |e: &dyn std::fmt::Display| MicError::Build(e.to_string());
        let desc = format!(
            "alsasrc device={device} ! audio/x-raw,format={fmt},rate={rate},channels={ch},layout=interleaved ! \
             tee name=tee allow-not-linked=true ! queue max-size-time=2000000000 ! appsink name=rec sync=false",
            fmt = format.format.gst_name(),
            rate = format.rate,
            ch = format.channels,
        );
        let pipeline = gst::parse::launch(&desc)
            .map_err(|e| build(&e))?
            .downcast::<gst::Pipeline>()
            .map_err(|_| build(&"not a pipeline"))?;
        // Same real-time clock as the cameras: base_time + running time = Unix time (BWF TimeReference).
        let clock = gst::SystemClock::obtain();
        clock.set_property_from_str("clock-type", "realtime");
        pipeline.use_clock(Some(&clock));

        let (registration, stream) = match (&mic.stream, mic.config.stream.enabled) {
            (Some(hub), true) => {
                let options =
                    AudioSourceOptions { echo_cancellation: false, noise_suppression: false, auto_gain_control: false };
                let source = NativeAudioSource::new(options, format.rate, format.channels, 100);
                let registration = hub.register_audio(&mic.config.name, source.clone(), mic.config.stream.bitrate_kbps);
                let tap = crate::stream::audio::spawn(
                    source,
                    registration.published.clone(),
                    format.format,
                    format.rate,
                    format.channels,
                );
                (Some(registration), Some(tap))
            }
            _ => (None, None),
        };
        let capture = Self {
            recorder: Arc::new(Mutex::new(Recorder {
                name: mic.config.name.clone(),
                dir: mic.recordings_dir.clone(),
                format,
                gate: mic.recording.clone(),
                file: None,
                retry_at: None,
                stream,
                health: mic.health.clone(),
            })),
            _registration: registration,
            last_buffer: Arc::new(AtomicU64::new(0)),
            epoch: Instant::now(),
            pipeline,
        };
        capture.connect_sink()?;

        let started = capture.pipeline.set_state(gst::State::Playing).is_ok();
        let bus = capture.pipeline.bus().ok_or_else(|| build(&"pipeline without bus"))?;
        let deadline = Instant::now() + START_TIMEOUT;
        while capture.last_buffer.load(Ordering::Relaxed) == 0 {
            let error = bus.pop_filtered(&[gst::MessageType::Error]).map(|m| describe(&m));
            if error.is_some() || !started || Instant::now() > deadline {
                capture.stop().await;
                return Err(error.map_or(MicError::Stalled(START_TIMEOUT), MicError::Pipeline));
            }
            tokio::time::sleep(Duration::from_millis(50)).await;
        }
        tracing::info!(%format, %device, "capture started");
        mic.health.started();
        Ok(capture)
    }

    async fn run(self, mic: &Microphone, token: CancellationToken) -> Result<(), MicError> {
        let result = self.watch(mic, token).await;
        self.stop().await;
        result
    }

    async fn watch(&self, mic: &Microphone, token: CancellationToken) -> Result<(), MicError> {
        let bus = self.pipeline.bus().ok_or_else(|| MicError::Build("pipeline without bus".into()))?;
        let mut messages = bus.stream();
        let mut gate = mic.recording.clone();
        let mut tick = tokio::time::interval(Duration::from_secs(1));
        loop {
            tokio::select! {
                () = token.cancelled() => return Ok(()),
                changed = gate.changed() => {
                    if changed.is_err() {
                        token.cancelled().await;
                        return Ok(());
                    }
                    if !*gate.borrow_and_update() {
                        // Close now rather than on the next samples (the writer reopens by itself).
                        lock(&self.recorder).close();
                    }
                }
                msg = messages.next() => match msg {
                    Some(msg) => match msg.view() {
                        gst::MessageView::Error(_) => return Err(MicError::Pipeline(describe(&msg))),
                        gst::MessageView::Warning(w) => {
                            tracing::warn!(warning = %w.error(), debug = ?w.debug(), "pipeline warning");
                        }
                        _ => {}
                    },
                    None => return Err(MicError::Pipeline("bus closed".into())),
                },
                _ = tick.tick() => {
                    let last = Duration::from_millis(self.last_buffer.load(Ordering::Relaxed));
                    let idle = self.epoch.elapsed().saturating_sub(last);
                    if idle > STALL_TIMEOUT {
                        return Err(MicError::Stalled(idle));
                    }
                }
            }
        }
    }

    /// Stops the capture, then finalizes the current file.
    async fn stop(&self) {
        let pipeline = self.pipeline.clone();
        let _ = tokio::task::spawn_blocking(move || pipeline.set_state(gst::State::Null)).await;
        lock(&self.recorder).close();
    }

    fn connect_sink(&self) -> Result<(), MicError> {
        let sink = self
            .pipeline
            .by_name("rec")
            .and_then(|e| e.downcast::<gst_app::AppSink>().ok())
            .ok_or_else(|| MicError::Build("appsink missing".into()))?;
        let (pipeline, recorder, last_buffer, epoch) =
            (self.pipeline.downgrade(), self.recorder.clone(), self.last_buffer.clone(), self.epoch);
        let health = lock(&self.recorder).health.clone();
        sink.set_callbacks(
            gst_app::AppSinkCallbacks::builder()
                .new_sample(move |sink| {
                    let sample = sink.pull_sample().map_err(|_| gst::FlowError::Eos)?;
                    last_buffer.store(epoch.elapsed().as_millis().max(1) as u64, Ordering::Relaxed);
                    health.frame();
                    let (Some(buffer), Some(pipeline)) = (sample.buffer(), pipeline.upgrade()) else {
                        return Ok(gst::FlowSuccess::Ok);
                    };
                    let capture_ns = match (pipeline.base_time(), buffer.pts()) {
                        (Some(base), Some(pts)) => {
                            let segment = sample.segment().and_then(|s| s.downcast_ref::<gst::ClockTime>().cloned());
                            Some(timecode::capture_unix_ns(base, segment.as_ref(), pts))
                        }
                        _ => None,
                    };
                    if let Ok(map) = buffer.map_readable() {
                        lock(&recorder).on_samples(capture_ns, map.as_slice());
                    }
                    Ok(gst::FlowSuccess::Ok)
                })
                .build(),
        );
        Ok(())
    }
}

struct OpenFile {
    writer: BwfWriter,
    path: PathBuf,
}

/// Writes the samples to BWF files according to the recording gate. Runs in the appsink streaming thread;
/// never fails: file errors are logged and retried after a delay.
struct Recorder {
    name: String,
    dir: PathBuf,
    format: AudioFormat,
    gate: watch::Receiver<bool>,
    file: Option<OpenFile>,
    retry_at: Option<Instant>,
    stream: Option<AudioTap>,
    health: Health,
}

impl Recorder {
    fn on_samples(&mut self, capture_ns: Option<u64>, pcm: &[u8]) {
        if let Some(tap) = &self.stream {
            tap.push(pcm);
        }
        if !*self.gate.borrow() {
            self.close();
            return;
        }
        if self.file.is_none() {
            if self.retry_at.is_some_and(|t| Instant::now() < t) {
                return;
            }
            self.retry_at = None;
            // The file starts with these samples: their capture time is the BWF TimeReference.
            let Some(capture_ns) = capture_ns else { return };
            if let Err(e) = self.open(capture_ns) {
                tracing::error!(error = %e, retry_s = RECORD_RETRY.as_secs(), "cannot start recording");
                self.health.failed(&format!("cannot start recording: {e}"));
                self.retry_at = Some(Instant::now() + RECORD_RETRY);
                return;
            }
        }
        if let Some(file) = &mut self.file
            && let Err(e) = file.writer.write(pcm)
        {
            tracing::error!(
                path = %file.path.display(),
                error = %e,
                retry_s = RECORD_RETRY.as_secs(),
                "recording write failed"
            );
            self.health.failed(&format!("recording write failed: {e}"));
            self.close();
            self.retry_at = Some(Instant::now() + RECORD_RETRY);
        }
    }

    fn open(&mut self, capture_ns: u64) -> std::io::Result<()> {
        let start = timecode::local_time(capture_ns);
        let path = new_file_path(&self.dir, &self.name, &chrono::Local::now(), "wav")?;
        let spec = BwfSpec {
            name: &self.name,
            sample_rate: self.format.rate,
            channels: self.format.channels as u16,
            bits: self.format.format.bits(),
            time_reference: timecode::samples_since_midnight(&start, self.format.rate),
            origination: start,
            timecode_rate: TIMECODE_RATE,
        };
        let writer = BwfWriter::create(&path, &spec)?;
        tracing::info!(
            path = %path.display(),
            start = %start.format("%H:%M:%S%.3f"),
            time_reference = spec.time_reference,
            "recording started"
        );
        self.file = Some(OpenFile { writer, path });
        Ok(())
    }

    fn close(&mut self) {
        let Some(file) = self.file.take() else { return };
        let duration_s = file.writer.duration_s();
        match file.writer.finalize() {
            Ok(()) => tracing::info!(path = %file.path.display(), duration_s, "recording finalized"),
            Err(e) => tracing::warn!(path = %file.path.display(), error = %e, "recording closed with an error"),
        }
    }
}

/// Locks the recorder even if a previous holder panicked (its state stays usable).
fn lock(recorder: &Mutex<Recorder>) -> std::sync::MutexGuard<'_, Recorder> {
    recorder.lock().unwrap_or_else(std::sync::PoisonError::into_inner)
}
