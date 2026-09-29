//! Effective per-device configuration: the `.env` defaults with the `devices.yml` override applied.

use super::devices::{CameraOverride, MicrophoneOverride};
use super::settings::Settings;
use super::types::{Resolution, Rotation};

#[derive(Debug, Clone, PartialEq)]
pub struct CameraConfig {
    pub name: String,
    pub rotation: Rotation,
    pub main: bool,
    pub max_resolution: Resolution,
    pub max_fps: u32,
    pub video_bitrate_kbps: u32,
    pub stream: StreamVideoConfig,
}

#[derive(Debug, Clone, PartialEq)]
pub struct StreamVideoConfig {
    pub enabled: bool,
    pub resolution: Resolution,
    pub fps: u32,
    pub bitrate_kbps: u32,
}

#[derive(Debug, Clone, PartialEq)]
pub struct MicConfig {
    pub name: String,
    /// Preferred sample rate, used if the microphone supports it.
    pub sample_rate: u32,
    /// Requested channel count / bit depth, used if the microphone supports them.
    pub channels: Option<u32>,
    pub bit_depth: Option<u32>,
    pub stream: StreamAudioConfig,
}

#[derive(Debug, Clone, PartialEq)]
pub struct StreamAudioConfig {
    pub enabled: bool,
    pub bitrate_kbps: u32,
}

impl CameraConfig {
    pub fn resolve(settings: &Settings, name: String, ov: Option<&CameraOverride>) -> Self {
        let v = &settings.video;
        let s = ov.and_then(|o| o.stream.as_ref());
        Self {
            name,
            rotation: ov.and_then(|o| o.rotation).unwrap_or_default(),
            main: ov.is_some_and(|o| o.main),
            max_resolution: v.max_resolution,
            max_fps: v.max_fps,
            video_bitrate_kbps: ov.and_then(|o| o.video_bitrate_kbps).unwrap_or(v.bitrate_kbps),
            stream: StreamVideoConfig {
                enabled: s.and_then(|s| s.enabled).unwrap_or(v.stream.enabled),
                resolution: s.and_then(|s| s.resolution).unwrap_or(v.stream.resolution),
                fps: s.and_then(|s| s.fps).unwrap_or(v.stream.fps),
                bitrate_kbps: s.and_then(|s| s.bitrate_kbps).unwrap_or(v.stream.bitrate_kbps),
            },
        }
    }
}

impl MicConfig {
    pub fn resolve(settings: &Settings, name: String, ov: Option<&MicrophoneOverride>) -> Self {
        let a = &settings.audio;
        let s = ov.and_then(|o| o.stream.as_ref());
        Self {
            name,
            sample_rate: a.sample_rate,
            channels: ov.and_then(|o| o.channels),
            bit_depth: ov.and_then(|o| o.bit_depth),
            stream: StreamAudioConfig {
                enabled: s.and_then(|s| s.enabled).unwrap_or(a.stream_enabled),
                bitrate_kbps: s.and_then(|s| s.bitrate_kbps).unwrap_or(a.stream_bitrate_kbps),
            },
        }
    }
}
