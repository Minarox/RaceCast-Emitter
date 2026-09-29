//! Camera task. The capture pipeline runs as long as the camera is plugged in:
//!
//! ```text
//! v4l2src (MJPEG) → nvv4l2decoder → videorate (constant fps, NVMM) → nvvidconv (rotation) → tee
//! ```
//!
//! While the recording gate is open, a recording branch is attached to the tee (H.264 → h264parse →
//! timecode probe → fragmented qtmux → file). Closing the gate detaches it with an EOS so that qtmux
//! finalizes the file, without stopping the capture.
//!
//! While the camera's LiveKit track is published, a stream branch hangs off the same tee (leaky queue →
//! frame-rate cap → `nvvidconv` downscale → appsink) and hands the NVMM surfaces to the SDK's Jetson AV1
//! encoder (zero copy, see `stream::video`).

use std::path::PathBuf;
use std::sync::atomic::{AtomicU64, Ordering};
use std::sync::{Arc, Mutex};
use std::time::{Duration, Instant};

use futures_util::StreamExt;
use gstreamer as gst;
use gstreamer::prelude::*;
use gstreamer_app as gst_app;
use gstreamer_video as gst_video;
use livekit::webrtc::prelude::VideoResolution;
use livekit::webrtc::video_source::native::NativeVideoSource;
use tokio::sync::{oneshot, watch};
use tokio_util::sync::CancellationToken;

use super::{RECORD_RETRY, STALL_TIMEOUT, START_TIMEOUT, describe, new_file_path, timecode};
use crate::capture::DeviceInfo;
use crate::capture::health::Health;
use crate::capture::modes::{self, VideoMode};
use crate::config::CameraConfig;
use crate::stream::video::{self as stream_video, VideoFeed};
use crate::stream::{Registration, StreamHub};

/// Time allowed for qtmux to finalize the file after the EOS.
const FINALIZE_TIMEOUT: Duration = Duration::from_secs(10);
/// Extra decoder surfaces. NVMM buffers downstream keep their source decoder surface referenced: every buffer
/// held by the branches (recording queue, stream queue, surfaces held for the LiveKit encoder) pins one.
/// With the driver default (+1), holding 4 stream frames starves the decoder and freezes the capture.
const DECODER_EXTRA_SURFACES: u32 = 10;

pub struct Camera {
    pub info: DeviceInfo,
    pub config: CameraConfig,
    pub recordings_dir: PathBuf,
    /// Recording gate (requested and enough disk space).
    pub recording: watch::Receiver<bool>,
    /// LiveKit streaming, if configured.
    pub stream: Option<Arc<StreamHub>>,
    /// Start, frames and failures, for the confirmation of configuration changes.
    pub health: Health,
}

#[derive(Debug, thiserror::Error)]
pub enum CameraError {
    #[error("cannot read the camera modes: {0}")]
    Probe(String),
    #[error("cannot build the pipeline: {0}")]
    Build(String),
    #[error("pipeline error: {0}")]
    Pipeline(String),
    #[error("no frame received for {0:?}")]
    Stalled(Duration),
}

/// Runs the camera until `token` is cancelled (camera unplugged, configuration change, shutdown).
pub async fn run(cam: Arc<Camera>, token: CancellationToken) -> Result<(), CameraError> {
    let devnode = cam.info.devnode.clone();
    let caps = tokio::task::spawn_blocking(move || probe_caps(&devnode))
        .await
        .map_err(|e| CameraError::Probe(e.to_string()))??;
    let all = modes::video_modes(&caps);
    let candidates = modes::video_candidates(&all, cam.config.max_resolution, cam.config.max_fps);
    if candidates.is_empty() {
        let available: Vec<String> = all.iter().map(ToString::to_string).collect();
        tracing::error!(
            available = %available.join(", "),
            cap = %format!("{}@{}", cam.config.max_resolution, cam.config.max_fps),
            "no MJPEG mode within the cap: camera ignored"
        );
        return Ok(());
    }
    for (i, mode) in candidates.iter().enumerate() {
        match Session::start(&cam, *mode).await {
            Ok(session) => return session.run(&cam, token).await,
            Err(StartError::Bandwidth(e)) if i + 1 < candidates.len() => {
                tracing::warn!(%mode, next = %candidates[i + 1], error = %e, "not enough USB bandwidth, stepping down");
            }
            Err(StartError::Bandwidth(e) | StartError::Other(e)) => return Err(e),
        }
    }
    Ok(())
}

fn probe_caps(devnode: &str) -> Result<gst::Caps, CameraError> {
    let err = |e: &dyn std::fmt::Display| CameraError::Probe(e.to_string());
    let src = gst::ElementFactory::make("v4l2src").property("device", devnode).build().map_err(|e| err(&e))?;
    src.set_state(gst::State::Ready).map_err(|e| err(&e))?;
    let caps = src.static_pad("src").map(|p| p.query_caps(None));
    let _ = src.set_state(gst::State::Null);
    caps.ok_or_else(|| err(&"v4l2src has no source pad"))
}

enum StartError {
    /// The camera could not start in this mode for lack of USB bandwidth: a lower mode may work.
    Bandwidth(CameraError),
    Other(CameraError),
}

struct Branch {
    bin: gst::Bin,
    tee_pad: gst::Pad,
    eos: oneshot::Receiver<()>,
    path: PathBuf,
    started: Instant,
}

/// LiveKit side of the camera: registration in the hub and, while published, the stream branch.
struct Stream {
    registration: Registration,
    feed: Arc<VideoFeed>,
    width: u32,
    height: u32,
    fps: u32,
    branch: Option<(gst::Bin, gst::Pad)>,
    retry_at: Option<Instant>,
}

struct Session {
    pipeline: gst::Pipeline,
    tee: gst::Element,
    mode: VideoMode,
    /// Frame size after rotation.
    size: (u32, u32),
    stream: Option<Stream>,
    /// Milliseconds since `epoch` of the last buffer that reached the tee (0 = none yet).
    last_frame: Arc<AtomicU64>,
    epoch: Instant,
    branch: Option<Branch>,
    retry_at: Option<Instant>,
    health: Health,
}

impl Session {
    async fn start(cam: &Camera, mode: VideoMode) -> Result<Self, StartError> {
        let build = |e: &dyn std::fmt::Display| StartError::Other(CameraError::Build(e.to_string()));
        let rotation = cam.config.rotation;
        let (out_w, out_h) =
            if rotation.swaps_dimensions() { (mode.height, mode.width) } else { (mode.width, mode.height) };
        let rate = format!("{}/{}", mode.fps_n, mode.fps_d);
        let desc = format!(
            "v4l2src name=src device={dev} ! image/jpeg,width={w},height={h},framerate={rate} ! \
             nvv4l2decoder mjpeg=1 num-extra-surfaces={extra} ! videorate skip-to-first=true ! video/x-raw(memory:NVMM),framerate={rate} ! \
             nvvidconv flip-method={flip} ! video/x-raw(memory:NVMM),format=NV12,width={out_w},height={out_h} ! \
             tee name=tee allow-not-linked=true",
            dev = cam.info.devnode,
            w = mode.width,
            h = mode.height,
            flip = rotation.nvvidconv_flip_method(),
            extra = DECODER_EXTRA_SURFACES,
        );
        let pipeline = gst::parse::launch(&desc)
            .map_err(|e| build(&e))?
            .downcast::<gst::Pipeline>()
            .map_err(|_| build(&"not a pipeline"))?;
        // Capture timestamps on the real-time clock: base_time + running time = Unix time (timecode).
        let clock = gst::SystemClock::obtain();
        clock.set_property_from_str("clock-type", "realtime");
        pipeline.use_clock(Some(&clock));

        let tee = pipeline.by_name("tee").ok_or_else(|| build(&"tee missing"))?;
        let epoch = Instant::now();
        let last_frame = Arc::new(AtomicU64::new(0));
        if let Some(pad) = tee.static_pad("sink") {
            let (last, health) = (last_frame.clone(), cam.health.clone());
            pad.add_probe(gst::PadProbeType::BUFFER, move |_, _| {
                last.store(epoch.elapsed().as_millis().max(1) as u64, Ordering::Relaxed);
                health.frame();
                gst::PadProbeReturn::Ok
            });
        }

        let session = Self {
            pipeline,
            tee,
            mode,
            size: (out_w, out_h),
            stream: None,
            last_frame,
            epoch,
            branch: None,
            retry_at: None,
            health: cam.health.clone(),
        };
        let started = session.pipeline.set_state(gst::State::Playing);
        let bus = session.pipeline.bus().ok_or_else(|| build(&"pipeline without bus"))?;
        let deadline = Instant::now() + START_TIMEOUT;
        loop {
            if let Some(msg) = bus.pop_filtered(&[gst::MessageType::Error]) {
                let error = describe(&msg);
                session.shutdown().await;
                let err = CameraError::Pipeline(error.clone());
                let bandwidth = ["No space left on device", "Failed to allocate"].iter().any(|s| error.contains(s));
                return Err(if bandwidth { StartError::Bandwidth(err) } else { StartError::Other(err) });
            }
            if session.last_frame.load(Ordering::Relaxed) > 0 {
                break;
            }
            if Instant::now() > deadline {
                session.shutdown().await;
                let err = match started {
                    Err(_) => CameraError::Pipeline("the pipeline refused to start".into()),
                    Ok(_) => CameraError::Stalled(START_TIMEOUT),
                };
                return Err(StartError::Other(err));
            }
            tokio::time::sleep(Duration::from_millis(50)).await;
        }
        tracing::info!(
            %mode,
            rotation = u16::from(rotation),
            devnode = %cam.info.devnode,
            "capture started"
        );
        cam.health.started();
        Ok(session)
    }

    async fn run(mut self, cam: &Camera, token: CancellationToken) -> Result<(), CameraError> {
        let Some(bus) = self.pipeline.bus() else {
            self.shutdown().await;
            return Err(CameraError::Build("pipeline without bus".into()));
        };
        let mut messages = bus.stream();
        let mut gate = cam.recording.clone();
        let mut tick = tokio::time::interval(Duration::from_secs(1));
        let open = *gate.borrow_and_update();
        self.sync_recording(cam, open).await;
        self.register_stream(cam);
        let mut published = self.stream.as_ref().map(|s| s.registration.published.clone());
        loop {
            tokio::select! {
                () = token.cancelled() => {
                    self.shutdown().await;
                    return Ok(());
                }
                changed = async {
                    match published.as_mut() {
                        Some(p) => p.changed().await,
                        None => std::future::pending().await,
                    }
                } => {
                    if changed.is_err() {
                        published = None;
                        continue;
                    }
                    self.sync_stream().await;
                }
                changed = gate.changed() => {
                    if changed.is_err() {
                        // Gate owner gone: the program is shutting down, the token follows.
                        token.cancelled().await;
                        continue;
                    }
                    let open = *gate.borrow_and_update();
                    self.sync_recording(cam, open).await;
                }
                msg = messages.next() => {
                    let Some(msg) = msg else {
                        self.shutdown().await;
                        return Err(CameraError::Pipeline("bus closed".into()));
                    };
                    if let Err(e) = self.handle_message(&msg).await {
                        self.shutdown().await;
                        return Err(e);
                    }
                }
                _ = tick.tick() => {
                    let idle = self.epoch.elapsed().saturating_sub(Duration::from_millis(self.last_frame.load(Ordering::Relaxed)));
                    if idle > STALL_TIMEOUT {
                        self.shutdown().await;
                        return Err(CameraError::Stalled(idle));
                    }
                    if self.retry_at.is_some_and(|t| Instant::now() >= t) {
                        self.retry_at = None;
                        let open = *gate.borrow();
                        self.sync_recording(cam, open).await;
                    }
                    if self.stream.as_ref().is_some_and(|s| s.retry_at.is_some_and(|t| Instant::now() >= t)) {
                        self.sync_stream().await;
                    }
                }
            }
        }
    }

    async fn handle_message(&mut self, msg: &gst::Message) -> Result<(), CameraError> {
        match msg.view() {
            gst::MessageView::Error(_) => {
                let error = describe(msg);
                let Some(src) = msg.src() else { return Err(CameraError::Pipeline(error)) };
                if !src.has_as_ancestor(&self.pipeline) {
                    // An element of a branch already removed (a branch that failed to start posts its error
                    // after being dropped): that failure was handled then, the capture is fine.
                    tracing::debug!(%error, "error from a removed branch ignored");
                    return Ok(());
                }
                // Capture chain elements are direct children of the pipeline; every branch is a bin.
                let Some(bin) = branch_bin(&self.pipeline, src) else { return Err(CameraError::Pipeline(error)) };
                let in_branch = self.branch.as_ref().is_some_and(|b| b.bin == bin);
                let in_stream = self.stream.as_ref().and_then(|s| s.branch.as_ref()).is_some_and(|(b, _)| *b == bin);
                if in_stream {
                    // Streaming failed: drop the stream branch, recording goes on, retry later.
                    tracing::warn!(%error, retry_s = RECORD_RETRY.as_secs(), "stream branch failed");
                    self.health.failed(&format!("stream branch failed: {error}"));
                    self.detach_stream().await;
                    if let Some(s) = &mut self.stream {
                        s.retry_at = Some(Instant::now() + RECORD_RETRY);
                    }
                    Ok(())
                } else if in_branch {
                    // Recording failed (e.g. disk write error): drop the branch, keep capturing, retry later.
                    tracing::error!(%error, retry_s = RECORD_RETRY.as_secs(), "recording branch failed");
                    self.health.failed(&format!("recording branch failed: {error}"));
                    self.drop_branch().await;
                    self.retry_at = Some(Instant::now() + RECORD_RETRY);
                    Ok(())
                } else {
                    // A branch being finalized or detached: it is on its way out, the capture is fine.
                    tracing::warn!(%error, "error in a branch being removed");
                    Ok(())
                }
            }
            gst::MessageView::Warning(w) => {
                let src = msg.src().map(|s| s.path_string().to_string()).unwrap_or_default();
                tracing::warn!(%src, warning = %w.error(), debug = ?w.debug(), "pipeline warning");
                Ok(())
            }
            _ => Ok(()),
        }
    }

    /// Registers the camera's LiveKit source (if streaming is configured and enabled for this camera).
    fn register_stream(&mut self, cam: &Camera) {
        let (Some(hub), true) = (&cam.stream, cam.config.stream.enabled) else { return };
        let s = &cam.config.stream;
        let (width, height) = stream_video::fit(self.size.0, self.size.1, s.resolution.width, s.resolution.height);
        let fps = s.fps.min(self.mode.fps_rounded());
        let source = NativeVideoSource::new(VideoResolution { width, height }, false);
        let registration = hub.register_video(&cam.config.name, cam.config.main, source.clone(), fps, s.bitrate_kbps);
        tracing::info!(size = %format!("{width}x{height}"), fps, bitrate_kbps = s.bitrate_kbps, "stream registered");
        self.stream = Some(Stream {
            registration,
            feed: Arc::new(VideoFeed::new(source, width, height)),
            width,
            height,
            fps,
            branch: None,
            retry_at: None,
        });
    }

    /// Runs the stream branch exactly while the track is published.
    async fn sync_stream(&mut self) {
        let Some(s) = &mut self.stream else { return };
        let published = *s.registration.published.borrow_and_update();
        if published && s.branch.is_none() {
            if s.retry_at.is_some_and(|t| Instant::now() < t) {
                return;
            }
            s.retry_at = None;
            if let Err(e) = self.attach_stream() {
                tracing::warn!(error = %e, retry_s = RECORD_RETRY.as_secs(), "cannot start the stream branch");
                self.health.failed(&format!("cannot start the stream branch: {e}"));
                if let Some(s) = &mut self.stream {
                    s.retry_at = Some(Instant::now() + RECORD_RETRY);
                }
            }
        } else if !published && s.branch.is_some() {
            self.detach_stream().await;
        }
    }

    fn attach_stream(&mut self) -> Result<(), String> {
        let Some(s) = &mut self.stream else { return Ok(()) };
        let desc = format!(
            "queue leaky=downstream max-size-buffers=2 max-size-bytes=0 max-size-time=0 !              videorate drop-only=true max-rate={fps} ! nvvidconv output-buffers={pool} !              video/x-raw(memory:NVMM),format=NV12,width={w},height={h} !              appsink name=sink max-buffers=2 drop=true sync=false",
            fps = s.fps,
            pool = stream_video::POOL_SIZE,
            w = s.width,
            h = s.height,
        );
        let bin = gst::parse::bin_from_description(&desc, true).map_err(|e| e.to_string())?;
        let sink = bin.by_name("sink").and_then(|e| e.downcast::<gst_app::AppSink>().ok()).ok_or("appsink missing")?;
        let feed = s.feed.clone();
        let warned = std::sync::atomic::AtomicBool::new(false);
        sink.set_callbacks(
            gst_app::AppSinkCallbacks::builder()
                .new_sample(move |sink| {
                    let sample = sink.pull_sample().map_err(|_| gst::FlowError::Eos)?;
                    if !feed.push(sample) && !warned.swap(true, Ordering::Relaxed) {
                        tracing::warn!("stream frame is not an NVMM surface: dropped");
                    }
                    Ok(gst::FlowSuccess::Ok)
                })
                .build(),
        );
        self.pipeline.add(&bin).map_err(|e| e.to_string())?;
        let linked = (|| {
            bin.sync_state_with_parent().map_err(|e| e.to_string())?;
            let tee_pad = self.tee.request_pad_simple("src_%u").ok_or("no tee pad")?;
            let bin_sink = bin.static_pad("sink").ok_or("branch has no sink pad")?;
            if let Err(e) = tee_pad.link_full(&bin_sink, gst::PadLinkCheck::empty()) {
                self.tee.release_request_pad(&tee_pad);
                return Err(format!("cannot link the stream branch: {e:?}"));
            }
            Ok(tee_pad)
        })();
        match linked {
            Ok(tee_pad) => {
                tracing::info!("streaming started");
                s.branch = Some((bin, tee_pad));
                Ok(())
            }
            Err(e) => {
                let _ = bin.set_state(gst::State::Null);
                let _ = self.pipeline.remove(&bin);
                Err(e)
            }
        }
    }

    /// Removes the stream branch (no EOS needed: nothing to finalize).
    async fn detach_stream(&mut self) {
        let Some(s) = &mut self.stream else { return };
        let Some((bin, tee_pad)) = s.branch.take() else { return };
        let feed = s.feed.clone();
        if let Some(peer) = tee_pad.peer() {
            let _ = tee_pad.unlink(&peer);
        }
        self.tee.release_request_pad(&tee_pad);
        self.remove_bin(bin).await;
        feed.release();
        tracing::info!("streaming stopped");
    }

    async fn sync_recording(&mut self, cam: &Camera, open: bool) {
        match (open, self.branch.is_some()) {
            (true, false) if self.retry_at.is_none() => {
                if let Err(e) = self.attach(cam) {
                    tracing::error!(error = %e, retry_s = RECORD_RETRY.as_secs(), "cannot start recording");
                    self.health.failed(&format!("cannot start recording: {e}"));
                    self.retry_at = Some(Instant::now() + RECORD_RETRY);
                }
            }
            (false, true) => self.finalize().await,
            _ => {}
        }
    }

    fn attach(&mut self, cam: &Camera) -> Result<(), String> {
        let path = new_file_path(&cam.recordings_dir, &cam.config.name, &chrono::Local::now(), "mov")
            .map_err(|e| format!("cannot create the recording directory: {e}"))?;
        let fps = self.mode.fps_rounded();
        let desc = format!(
            "queue max-size-buffers=4 max-size-bytes=0 max-size-time=0 ! \
             nvv4l2h264enc bitrate={bps} control-rate=0 profile=4 iframeinterval={fps} idrinterval={fps} \
             insert-sps-pps=true ! h264parse name=parse ! \
             qtmux fragment-duration=1000 fragment-mode=first-moov-then-finalise ! \
             filesink name=sink sync=false async=false buffer-mode=2",
            bps = u64::from(cam.config.video_bitrate_kbps) * 1000,
        );
        let bin = gst::parse::bin_from_description(&desc, true).map_err(|e| e.to_string())?;
        let sink = bin.by_name("sink").ok_or("filesink missing")?;
        sink.set_property("location", path.to_string_lossy().as_ref());
        let parse_src = bin.by_name("parse").and_then(|p| p.static_pad("src")).ok_or("h264parse missing")?;
        add_timecode_probe(&parse_src, &self.pipeline, fps);
        let eos = eos_probe(&sink.static_pad("sink").ok_or("filesink has no sink pad")?);

        self.pipeline.add(&bin).map_err(|e| e.to_string())?;
        let linked = (|| {
            bin.sync_state_with_parent().map_err(|e| e.to_string())?;
            let tee_pad = self.tee.request_pad_simple("src_%u").ok_or("no tee pad")?;
            let bin_sink = bin.static_pad("sink").ok_or("branch has no sink pad")?;
            // No caps check: nvv4l2decoder answers caps queries coming back up through the tee with NULL
            // caps (link refused with NOFORMAT). Negotiation then happens normally through the caps event.
            if let Err(e) = tee_pad.link_full(&bin_sink, gst::PadLinkCheck::empty()) {
                self.tee.release_request_pad(&tee_pad);
                return Err(format!("cannot link the recording branch: {e:?}"));
            }
            Ok(tee_pad)
        })();
        match linked {
            Ok(tee_pad) => {
                tracing::info!(path = %path.display(), "recording started");
                self.branch = Some(Branch { bin, tee_pad, eos, path, started: Instant::now() });
                Ok(())
            }
            Err(e) => {
                let _ = bin.set_state(gst::State::Null);
                let _ = self.pipeline.remove(&bin);
                Err(e)
            }
        }
    }

    /// Detaches the recording branch with an EOS so that qtmux writes the final `moov`, then removes it.
    async fn finalize(&mut self) {
        let Some(branch) = self.branch.take() else { return };
        let (idle_tx, idle_rx) = oneshot::channel();
        let idle_tx = Mutex::new(Some(idle_tx));
        branch.tee_pad.add_probe(gst::PadProbeType::IDLE, move |pad, _| {
            unlink_with_eos(pad);
            if let Some(tx) = idle_tx.lock().ok().and_then(|mut t| t.take()) {
                let _ = tx.send(());
            }
            gst::PadProbeReturn::Remove
        });
        if tokio::time::timeout(Duration::from_secs(2), idle_rx).await.is_err() {
            unlink_with_eos(&branch.tee_pad);
        }
        self.tee.release_request_pad(&branch.tee_pad);
        let finalized = matches!(tokio::time::timeout(FINALIZE_TIMEOUT, branch.eos).await, Ok(Ok(())));
        self.remove_bin(branch.bin).await;
        let duration_s = branch.started.elapsed().as_secs();
        if finalized {
            tracing::info!(path = %branch.path.display(), duration_s, "recording finalized");
        } else {
            tracing::warn!(
                path = %branch.path.display(),
                duration_s,
                "recording closed without finalization (readable up to the last fragment)"
            );
        }
    }

    /// Removes a failed recording branch without waiting for an EOS (the file is readable up to the last
    /// fragment).
    async fn drop_branch(&mut self) {
        let Some(branch) = self.branch.take() else { return };
        if let Some(peer) = branch.tee_pad.peer() {
            let _ = branch.tee_pad.unlink(&peer);
        }
        self.tee.release_request_pad(&branch.tee_pad);
        self.remove_bin(branch.bin).await;
        tracing::warn!(path = %branch.path.display(), "recording interrupted");
    }

    async fn remove_bin(&self, bin: gst::Bin) {
        let pipeline = self.pipeline.clone();
        // NVENC teardown can take a while: off the async worker threads.
        let _ = tokio::task::spawn_blocking(move || {
            let _ = bin.set_state(gst::State::Null);
            let _ = pipeline.remove(&bin);
        })
        .await;
    }

    /// Stops streaming, finalizes the recording (if any) and stops the capture.
    async fn shutdown(mut self) {
        self.detach_stream().await;
        self.stream = None; // unregisters the track
        self.finalize().await;
        let pipeline = self.pipeline.clone();
        let _ = tokio::task::spawn_blocking(move || pipeline.set_state(gst::State::Null)).await;
    }
}

/// Unlinks a tee pad from its branch and sends the branch an EOS (from the pad's streaming context when
/// called in an IDLE probe).
/// The branch bin (direct child of the pipeline) that contains `src`, or `None` for an element of the
/// capture chain (or the pipeline itself).
fn branch_bin(pipeline: &gst::Pipeline, src: &gst::Object) -> Option<gst::Bin> {
    let pipeline = pipeline.upcast_ref::<gst::Object>();
    let mut current = src.clone();
    loop {
        let parent = current.parent()?;
        if parent == *pipeline {
            return current.downcast::<gst::Bin>().ok();
        }
        current = parent;
    }
}

fn unlink_with_eos(tee_pad: &gst::Pad) {
    if let Some(peer) = tee_pad.peer() {
        let _ = tee_pad.unlink(&peer);
        peer.send_event(gst::event::Eos::new());
    }
}

/// Signals when an EOS reaches `pad` (qtmux has written the final `moov` by then).
fn eos_probe(pad: &gst::Pad) -> oneshot::Receiver<()> {
    let (tx, rx) = oneshot::channel();
    let tx = Mutex::new(Some(tx));
    pad.add_probe(gst::PadProbeType::EVENT_DOWNSTREAM, move |_, info| {
        if let Some(gst::PadProbeData::Event(ev)) = &info.data
            && ev.type_() == gst::EventType::Eos
        {
            if let Some(tx) = tx.lock().ok().and_then(|mut t| t.take()) {
                let _ = tx.send(());
            }
            return gst::PadProbeReturn::Remove;
        }
        gst::PadProbeReturn::Ok
    });
    rx
}

/// Attaches the capture-time timecode to every H.264 buffer; qtmux turns it into the `tmcd` track.
fn add_timecode_probe(pad: &gst::Pad, pipeline: &gst::Pipeline, fps: u32) {
    let pipeline = pipeline.downgrade();
    let logged = std::sync::atomic::AtomicBool::new(false);
    pad.add_probe(gst::PadProbeType::BUFFER, move |pad, info| {
        let (Some(pipeline), Some(buffer)) = (pipeline.upgrade(), info.buffer_mut()) else {
            return gst::PadProbeReturn::Ok;
        };
        let (Some(base), Some(pts)) = (pipeline.base_time(), buffer.pts()) else {
            return gst::PadProbeReturn::Ok;
        };
        let segment = pad.sticky_event::<gst::event::Segment>(0);
        let segment = segment.as_ref().and_then(|e| e.segment().downcast_ref::<gst::ClockTime>().cloned());
        let t = timecode::local_time(timecode::capture_unix_ns(base, segment.as_ref(), pts));
        let (h, m, s, f) = timecode::timecode_fields(&t, fps);
        let tc = gst_video::VideoTimeCode::new(
            gst::Fraction::new(fps as i32, 1),
            None,
            gst_video::VideoTimeCodeFlags::empty(),
            h,
            m,
            s,
            f,
            0,
        );
        if let Ok(tc) = gst_video::ValidVideoTimeCode::try_from(tc) {
            if !logged.swap(true, Ordering::Relaxed) {
                tracing::info!(timecode = %tc, "recording timecode");
            }
            gst_video::VideoTimeCodeMeta::add(buffer.make_mut(), &tc);
        }
        gst::PadProbeReturn::Ok
    });
}
