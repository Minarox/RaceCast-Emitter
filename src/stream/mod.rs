//! LiveKit streaming (SPEC §6). Device tasks register their media source in the [`StreamHub`]; the room task
//! publishes every registered source while connected and tells each device, through its
//! [`Registration`], whether its track is published (the device only runs its stream branch then).
//! Telemetry goes to the room metadata (`metadata`).

pub mod audio;
pub mod metadata;
pub mod room;
pub mod video;

use std::collections::BTreeMap;
use std::sync::{Arc, Mutex, MutexGuard};

use livekit::webrtc::audio_source::native::NativeAudioSource;
use livekit::webrtc::video_source::native::NativeVideoSource;
use tokio::sync::watch;

use crate::capture::DeviceKind;

/// A media source handed to the room task.
#[derive(Clone)]
enum Media {
    Video { source: NativeVideoSource, fps: f64 },
    Audio { source: NativeAudioSource },
}

#[derive(Clone)]
struct Entry {
    name: String,
    main: bool,
    bitrate_bps: u64,
    media: Media,
    published: watch::Sender<bool>,
}

impl Entry {
    fn kind(&self) -> DeviceKind {
        match self.media {
            Media::Video { .. } => DeviceKind::Camera,
            Media::Audio { .. } => DeviceKind::Microphone,
        }
    }
}

/// Registry of the media sources to publish, shared by the device tasks and the room task.
pub struct StreamHub {
    state: Mutex<(u64, BTreeMap<u64, Entry>)>,
    /// Bumped on every registration change.
    changed: watch::Sender<u64>,
    /// Room connection state (also written in the `system` telemetry).
    connected: watch::Sender<bool>,
}

/// Keeps a source registered while alive; `published` tells whether its track is live in the room.
pub struct Registration {
    hub: Arc<StreamHub>,
    id: u64,
    pub published: watch::Receiver<bool>,
}

impl Drop for Registration {
    fn drop(&mut self) {
        self.hub.lock().1.remove(&self.id);
        self.hub.bump();
    }
}

/// Snapshot of a registered source, for the car state in the room metadata.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct StreamState {
    pub kind: DeviceKind,
    pub name: String,
    pub main: bool,
    pub published: bool,
}

impl StreamHub {
    pub fn new() -> Arc<Self> {
        Arc::new(Self {
            state: Mutex::new((0, BTreeMap::new())),
            changed: watch::channel(0).0,
            connected: watch::channel(false).0,
        })
    }

    fn lock(&self) -> MutexGuard<'_, (u64, BTreeMap<u64, Entry>)> {
        self.state.lock().unwrap_or_else(std::sync::PoisonError::into_inner)
    }

    fn bump(&self) {
        self.changed.send_modify(|v| *v = v.wrapping_add(1));
    }

    fn register(self: &Arc<Self>, name: &str, main: bool, bitrate_bps: u64, media: Media) -> Registration {
        let (published, rx) = watch::channel(false);
        let id = {
            let mut state = self.lock();
            state.0 += 1;
            let id = state.0;
            state.1.insert(id, Entry { name: name.to_string(), main, bitrate_bps, media, published });
            id
        };
        self.bump();
        Registration { hub: self.clone(), id, published: rx }
    }

    /// Registers a camera. Frames pushed into `source` reach the SDK's Jetson AV1 encoder.
    pub fn register_video(
        self: &Arc<Self>,
        name: &str,
        main: bool,
        source: NativeVideoSource,
        fps: u32,
        bitrate_kbps: u32,
    ) -> Registration {
        let media = Media::Video { source, fps: f64::from(fps) };
        self.register(name, main, u64::from(bitrate_kbps) * 1000, media)
    }

    /// Registers a microphone (Opus in the SDK).
    pub fn register_audio(self: &Arc<Self>, name: &str, source: NativeAudioSource, bitrate_kbps: u32) -> Registration {
        self.register(name, false, u64::from(bitrate_kbps) * 1000, Media::Audio { source })
    }

    fn entries(&self) -> BTreeMap<u64, Entry> {
        self.lock().1.clone()
    }

    /// Marks every source as unpublished (room lost).
    fn clear_published(&self) {
        for e in self.lock().1.values() {
            e.published.send_replace(false);
        }
    }

    pub fn connected(&self) -> watch::Receiver<bool> {
        self.connected.subscribe()
    }

    pub fn states(&self) -> Vec<StreamState> {
        self.lock()
            .1
            .values()
            .map(|e| StreamState {
                kind: e.kind(),
                name: e.name.clone(),
                main: e.main,
                published: *e.published.borrow(),
            })
            .collect()
    }
}
