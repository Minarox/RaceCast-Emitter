//! Capture devices: hot-plug detection (udev), mode selection and the device manager that starts one
//! supervised task per camera/microphone.

pub mod health;
pub mod manager;
pub mod modes;
mod naming;
pub mod udev;

use std::fmt;

#[derive(Debug, Clone, Copy, PartialEq, Eq, Hash)]
pub enum DeviceKind {
    Camera,
    Microphone,
}

impl fmt::Display for DeviceKind {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        f.write_str(match self {
            Self::Camera => "camera",
            Self::Microphone => "mic",
        })
    }
}

/// A USB capture device as seen by udev.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct DeviceInfo {
    pub kind: DeviceKind,
    /// Stable key for the lifetime of the plug (sysfs path).
    pub syspath: String,
    /// `/dev/videoN` for a camera, `/dev/snd/controlCN` for a microphone.
    pub devnode: String,
    /// Physical USB port, e.g. `platform-3610000.usb-usb-0:2.1` (`ID_PATH` without the interface).
    pub usb_port: Option<String>,
    /// Basename of the `/dev/v4l/by-id/…` or `/dev/snd/by-id/…` link.
    pub by_id: Option<String>,
    /// USB model name (`ID_MODEL`), used for automatic names.
    pub model: String,
    /// ALSA card identifier (e.g. `Camera`), microphones only.
    pub alsa_card: Option<String>,
}

/// A device currently handled by the manager (published for telemetry and, later, LiveKit).
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct ActiveDevice {
    pub kind: DeviceKind,
    pub name: String,
    pub main: bool,
}

/// Change reported by the udev watcher.
#[derive(Debug, Clone)]
pub enum DeviceEvent {
    Added(DeviceInfo),
    Removed {
        syspath: String,
    },
    /// The watcher (re)started: events may have been missed, the manager must re-enumerate.
    Resync,
}
