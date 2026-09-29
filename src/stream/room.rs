//! Room connection task: connects to LiveKit, publishes the registered sources, keeps the `main_camera`
//! participant attribute up to date, gives the main camera a larger share of a short uplink and checks that
//! video stays hardware-encoded. A lost connection ends
//! the task (the supervisor reconnects with backoff); the SDK's own short reconnections (resume) are only
//! logged.
//!
//! Never `mute()` a video track here: muting a track fed with DMA-BUF frames crashes the SDK (it tries to
//! turn the NVIDIA surface into black I420 frames). Stop a video by unpublishing it.

use std::collections::{BTreeMap, HashMap};
use std::time::Duration;

use livekit::options::{
    AudioEncoding, DegradationPreference, TrackPublishOptions, VideoCodec, VideoEncoderBackend, VideoEncoding,
};
use livekit::prelude::*;
use livekit::webrtc::prelude::{RtcAudioSource, RtcVideoSource};
use livekit::webrtc::stats::RtcStats;
use livekit_api::access_token::{AccessToken, VideoGrants};
use livekit_api::services::room::{CreateRoomOptions, RoomClient};
use tokio_util::sync::CancellationToken;

use super::{Entry, Media, StreamHub};
use crate::config::LiveKitSettings;

/// Room kept open this long after the car leaves, so that viewers keep the last telemetry.
const ROOM_TIMEOUT_S: u32 = 24 * 3600;
const STATS_PERIOD: Duration = Duration::from_secs(2);
/// WebRTC `bitrate_priority` of the main camera (the other tracks keep 1.0). When the uplink is short, every
/// stream first gets its minimum, then the rest is split in proportion to these priorities, up to each
/// stream's maximum: the main camera keeps its quality, the others give way (none is stopped). Needs the
/// SDK patch in `vendor/` (SPEC §6).
const MAIN_CAMERA_BITRATE_PRIORITY: f64 = 4.0;
const TOKEN_TTL: Duration = Duration::from_secs(24 * 3600);

struct Published {
    entry: Entry,
    publication: LocalTrackPublication,
    track: LocalTrack,
    encoder_checked: bool,
}

/// HTTP(S) base URL of the server API, from the WebSocket URL.
pub fn api_url(ws_url: &str) -> String {
    ws_url.replacen("wss://", "https://", 1).replacen("ws://", "http://", 1)
}

/// Creates the room if needed, with a long departure timeout (the car state must survive its absence).
pub async fn ensure_room(lk: &LiveKitSettings) -> Result<(), String> {
    let client = RoomClient::with_api_key(&api_url(&lk.url), &lk.api_key, &lk.api_secret);
    let options =
        CreateRoomOptions { empty_timeout: ROOM_TIMEOUT_S, departure_timeout: ROOM_TIMEOUT_S, ..Default::default() };
    client.create_room(&lk.room, options).await.map(|_| ()).map_err(|e| e.to_string())
}

fn token(lk: &LiveKitSettings) -> Result<String, String> {
    AccessToken::with_api_key(&lk.api_key, &lk.api_secret)
        .with_identity(&lk.identity)
        .with_name(&lk.identity)
        .with_ttl(TOKEN_TTL)
        .with_grants(VideoGrants {
            room_join: true,
            room: lk.room.clone(),
            can_publish: true,
            can_subscribe: false,
            can_publish_data: true,
            can_update_own_metadata: true,
            ..Default::default()
        })
        .to_jwt()
        .map_err(|e| e.to_string())
}

/// Sets the WebRTC `bitrate_priority` of a video track's sender; returns the value read back from libwebrtc.
fn set_bitrate_priority(track: &LocalVideoTrack, priority: f64) -> Result<f64, String> {
    let sender = track.transceiver().ok_or("track has no sender")?.sender();
    let mut parameters = sender.parameters();
    if parameters.encodings.is_empty() {
        return Err("sender has no encoding".into());
    }
    for encoding in &mut parameters.encodings {
        encoding.bitrate_priority = priority;
    }
    sender.set_parameters(parameters).map_err(|e| format!("{:?}: {}", e.error_type, e.message))?;
    Ok(sender.parameters().encodings.first().map_or(0.0, |e| e.bitrate_priority))
}

/// Connects and serves the room until the connection is lost or `token` is cancelled.
pub async fn run(hub: std::sync::Arc<StreamHub>, lk: LiveKitSettings, cancel: CancellationToken) -> Result<(), String> {
    if let Err(e) = ensure_room(&lk).await {
        tracing::warn!(error = %e, room = %lk.room, "cannot create the room (it may already exist)");
    }
    let mut options = RoomOptions::default();
    options.auto_subscribe = false;
    let (room, mut events) = Room::connect(&lk.url, &token(&lk)?, options)
        .await
        .map_err(|e| format!("cannot connect to {}: {e}", lk.url))?;
    tracing::info!(url = %lk.url, room = %lk.room, identity = %lk.identity, "connected to LiveKit");
    hub.connected.send_replace(true);

    let mut session = Session { room, published: HashMap::new(), main_camera: None };
    let mut changed = hub.changed.subscribe();
    let mut stats = tokio::time::interval(STATS_PERIOD);
    let result = loop {
        // Once shutting down, never unpublish: the devices unregister at the same time, and a renegotiation
        // racing the room close fails ("Called in wrong state: closed"). Closing the room ends every track.
        if cancel.is_cancelled() {
            break Ok(());
        }
        session.sync(&hub).await;
        tokio::select! {
            biased;
            () = cancel.cancelled() => break Ok(()),
            r = changed.changed() => if r.is_err() { break Ok(()) },
            event = events.recv() => match event {
                Some(RoomEvent::Disconnected { reason }) => break Err(format!("disconnected from LiveKit: {reason:?}")),
                Some(RoomEvent::Reconnecting) => tracing::info!("LiveKit connection interrupted, resuming"),
                Some(RoomEvent::Reconnected) => tracing::info!("LiveKit connection resumed"),
                Some(_) => {}
                None => break Err("LiveKit event stream closed".into()),
            },
            _ = stats.tick() => session.check_encoders().await,
        }
    };
    hub.connected.send_replace(false);
    hub.clear_published();
    let _ = tokio::time::timeout(Duration::from_secs(5), session.room.close()).await;
    result
}

struct Session {
    room: Room,
    published: HashMap<u64, Published>,
    main_camera: Option<String>,
}

impl Session {
    /// Publishes new sources, unpublishes removed ones, updates the `main_camera` attribute.
    async fn sync(&mut self, hub: &StreamHub) {
        let entries: BTreeMap<u64, Entry> = hub.entries();
        let participant = self.room.local_participant();
        let gone: Vec<u64> = self.published.keys().filter(|id| !entries.contains_key(id)).copied().collect();
        for id in gone {
            if let Some(p) = self.published.remove(&id) {
                if let Err(e) = participant.unpublish_track(&p.publication.sid()).await {
                    tracing::warn!(track = %p.entry.name, error = %e, "cannot unpublish track");
                }
                p.entry.published.send_replace(false);
                tracing::info!(track = %p.entry.name, "track unpublished");
            }
        }
        let new: Vec<(u64, Entry)> =
            entries.iter().filter(|(id, _)| !self.published.contains_key(id)).map(|(id, e)| (*id, e.clone())).collect();
        for (id, entry) in new {
            let (track, options) = match &entry.media {
                Media::Video { source, fps } => (
                    LocalTrack::Video(LocalVideoTrack::create_video_track(
                        &entry.name,
                        RtcVideoSource::Native(source.clone()),
                    )),
                    TrackPublishOptions {
                        source: TrackSource::Camera,
                        // AV1 + no simulcast: required for the SDK's Jetson hardware encoder (SPEC §6).
                        video_codec: VideoCodec::AV1,
                        simulcast: false,
                        video_encoder: VideoEncoderBackend::Auto,
                        video_encoding: Some(VideoEncoding { max_bitrate: entry.bitrate_bps, max_framerate: *fps }),
                        // Drop frames rather than downscale: downscaling a DMA-BUF frame would go through a
                        // CPU copy (the SDK's default for cameras, MaintainFramerate, lowers the resolution).
                        degradation_preference: Some(DegradationPreference::MaintainResolution),
                        ..Default::default()
                    },
                ),
                Media::Audio { source } => (
                    LocalTrack::Audio(LocalAudioTrack::create_audio_track(
                        &entry.name,
                        RtcAudioSource::Native(source.clone()),
                    )),
                    TrackPublishOptions {
                        source: TrackSource::Microphone,
                        audio_encoding: Some(AudioEncoding { max_bitrate: entry.bitrate_bps }),
                        dtx: false,
                        red: false,
                        ..Default::default()
                    },
                ),
            };
            match participant.publish_track(track.clone(), options).await {
                Ok(publication) => {
                    tracing::info!(track = %entry.name, main = entry.main, bitrate_kbps = entry.bitrate_bps / 1000, "track published");
                    if let (LocalTrack::Video(video), true) = (&track, entry.main) {
                        match set_bitrate_priority(video, MAIN_CAMERA_BITRATE_PRIORITY) {
                            Ok(applied) => {
                                tracing::info!(track = %entry.name, priority = applied, "main camera bitrate priority set")
                            }
                            Err(e) => {
                                tracing::warn!(track = %entry.name, error = %e, "cannot set the main camera bitrate priority")
                            }
                        }
                    }
                    entry.published.send_replace(true);
                    self.published
                        .insert(id, Published { entry: entry.clone(), publication, track, encoder_checked: false });
                }
                Err(e) => tracing::warn!(track = %entry.name, error = %e, "cannot publish track, retrying"),
            }
        }

        let main = entries.values().find(|e| e.main && matches!(e.media, Media::Video { .. })).map(|e| e.name.clone());
        if main != self.main_camera {
            let value = main.clone().unwrap_or_default();
            match participant.set_attributes(HashMap::from([("main_camera".to_string(), value)])).await {
                Ok(()) => self.main_camera = main,
                Err(e) => tracing::warn!(error = %e, "cannot set the main_camera attribute"),
            }
        }
    }

    /// Checks once per video track that the SDK's hardware encoder is used (a silent fallback to software
    /// would cost CPU and battery).
    async fn check_encoders(&mut self) {
        for p in self.published.values_mut().filter(|p| !p.encoder_checked) {
            let LocalTrack::Video(track) = &p.track else {
                p.encoder_checked = true;
                continue;
            };
            let Ok(stats) = track.get_stats().await else { continue };
            let encoder = stats.iter().find_map(|s| match s {
                RtcStats::OutboundRtp(o) if !o.outbound.encoder_implementation.is_empty() => {
                    Some(o.outbound.encoder_implementation.clone())
                }
                _ => None,
            });
            let Some(encoder) = encoder else { continue };
            p.encoder_checked = true;
            if encoder.contains("Jetson") {
                tracing::info!(track = %p.entry.name, %encoder, "hardware encoder in use");
            } else {
                tracing::error!(track = %p.entry.name, %encoder, "software video encoder in use (CPU cost, battery)");
            }
        }
    }
}
