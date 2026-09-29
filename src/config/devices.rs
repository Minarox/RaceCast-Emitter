//! `devices.yml`: optional per-device overrides (empty at first, SPEC §8).
//!
//! An invalid file is never applied: the active configuration stays the previous one. A valid file only
//! becomes active once the devices it changes are confirmed (`capture::manager`); the last confirmed
//! version is copied next to it (`<file>.last-good`) so that it survives a restart, and a rolled-back one
//! is kept as `<file>.rejected`.

use std::collections::HashSet;
use std::fs;
use std::io;
use std::path::{Path, PathBuf};
use std::sync::{Arc, Mutex};

use serde::{Deserialize, Serialize};

use super::types::{Resolution, Rotation, limits, validate_name};
use crate::storage;

#[derive(Debug, Clone, Default, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct DevicesFile {
    #[serde(default, skip_serializing_if = "Vec::is_empty")]
    pub cameras: Vec<CameraOverride>,
    #[serde(default, skip_serializing_if = "Vec::is_empty")]
    pub microphones: Vec<MicrophoneOverride>,
}

/// Overrides for one camera, identified by its physical USB port (`usb_path`, preferred) or by its
/// `by_id` identifier.
#[derive(Debug, Clone, Default, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct CameraOverride {
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub usb_path: Option<String>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub by_id: Option<String>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub name: Option<String>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub rotation: Option<Rotation>,
    /// Main camera: at most one.
    #[serde(default, skip_serializing_if = "std::ops::Not::not")]
    pub main: bool,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub video_bitrate_kbps: Option<u32>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub stream: Option<StreamVideoOverride>,
}

#[derive(Debug, Clone, Default, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct StreamVideoOverride {
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub enabled: Option<bool>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub resolution: Option<Resolution>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub fps: Option<u32>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub bitrate_kbps: Option<u32>,
}

#[derive(Debug, Clone, Default, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct MicrophoneOverride {
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub usb_path: Option<String>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub by_id: Option<String>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub name: Option<String>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub channels: Option<u32>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub bit_depth: Option<u32>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub stream: Option<StreamAudioOverride>,
}

#[derive(Debug, Clone, Default, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct StreamAudioOverride {
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub enabled: Option<bool>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub bitrate_kbps: Option<u32>,
}

#[derive(Debug, thiserror::Error)]
pub enum DevicesError {
    #[error("{path} unreadable: {source}")]
    Read { path: PathBuf, source: io::Error },
    #[error("{path}: invalid YAML: {message}")]
    Parse { path: PathBuf, message: String },
    #[error("{path}: {} error(s): {}", .errors.len(), .errors.join("; "))]
    Invalid { path: PathBuf, errors: Vec<String> },
}

impl DevicesFile {
    /// Parses and validates YAML content. Empty or comments only → no overrides.
    pub fn parse(path: &Path, text: &str) -> Result<Self, DevicesError> {
        let parsed: Option<Self> = serde_yaml_ng::from_str(text)
            .map_err(|e| DevicesError::Parse { path: path.to_path_buf(), message: e.to_string() })?;
        let file = parsed.unwrap_or_default();
        let errors = file.validate();
        if errors.is_empty() { Ok(file) } else { Err(DevicesError::Invalid { path: path.to_path_buf(), errors }) }
    }

    /// Override of the camera plugged into `usb_port` or identified by `by_id` (the port wins).
    pub fn camera(&self, usb_port: Option<&str>, by_id: Option<&str>) -> Option<&CameraOverride> {
        find(&self.cameras, usb_port, by_id, |c| (c.usb_path.as_deref(), c.by_id.as_deref()))
    }

    /// Override of the microphone plugged into `usb_port` or identified by `by_id` (the port wins).
    pub fn microphone(&self, usb_port: Option<&str>, by_id: Option<&str>) -> Option<&MicrophoneOverride> {
        find(&self.microphones, usb_port, by_id, |m| (m.usb_path.as_deref(), m.by_id.as_deref()))
    }

    /// Every name set by an override: automatic names must avoid them.
    pub fn reserved_names(&self) -> impl Iterator<Item = &str> {
        let cams = self.cameras.iter().filter_map(|c| c.name.as_deref());
        cams.chain(self.microphones.iter().filter_map(|m| m.name.as_deref()))
    }

    /// Rules the types cannot express: identification, uniqueness, bounds.
    fn validate(&self) -> Vec<String> {
        let mut errors = Vec::new();
        let mut names = HashSet::new();

        let mut keys = HashSet::new();
        for (i, cam) in self.cameras.iter().enumerate() {
            let at = format!("cameras[{i}]");
            check_identity(&at, cam.usb_path.as_deref(), cam.by_id.as_deref(), &mut keys, &mut errors);
            check_name(&at, cam.name.as_deref(), &mut names, &mut errors);
            check_range(&at, "video_bitrate_kbps", cam.video_bitrate_kbps, limits::VIDEO_BITRATE_KBPS, &mut errors);
            if let Some(s) = &cam.stream {
                check_range(&at, "stream.fps", s.fps, limits::FPS, &mut errors);
                check_range(&at, "stream.bitrate_kbps", s.bitrate_kbps, limits::STREAM_VIDEO_BITRATE_KBPS, &mut errors);
            }
        }
        let mains = self.cameras.iter().filter(|c| c.main).count();
        if mains > 1 {
            errors.push(format!("{mains} cameras marked \"main: true\" (at most one)"));
        }

        let mut keys = HashSet::new();
        for (i, mic) in self.microphones.iter().enumerate() {
            let at = format!("microphones[{i}]");
            check_identity(&at, mic.usb_path.as_deref(), mic.by_id.as_deref(), &mut keys, &mut errors);
            check_name(&at, mic.name.as_deref(), &mut names, &mut errors);
            check_range(&at, "channels", mic.channels, limits::CHANNELS, &mut errors);
            if let Some(bits) = mic.bit_depth
                && !limits::BIT_DEPTHS.contains(&bits)
            {
                errors.push(format!("{at}.bit_depth: invalid value {bits} (16, 24 or 32)"));
            }
            if let Some(s) = &mic.stream {
                check_range(&at, "stream.bitrate_kbps", s.bitrate_kbps, limits::STREAM_AUDIO_BITRATE_KBPS, &mut errors);
            }
        }
        errors
    }
}

fn find<'a, T>(
    entries: &'a [T],
    usb_port: Option<&str>,
    by_id: Option<&str>,
    keys: impl Fn(&T) -> (Option<&str>, Option<&str>),
) -> Option<&'a T> {
    let by_port = usb_port.and_then(|port| entries.iter().find(|e| keys(e).0 == Some(port)));
    by_port.or_else(|| by_id.and_then(|id| entries.iter().find(|e| keys(e).1 == Some(id))))
}

fn check_identity(
    at: &str,
    usb_path: Option<&str>,
    by_id: Option<&str>,
    seen: &mut HashSet<String>,
    errors: &mut Vec<String>,
) {
    let key = match (usb_path, by_id) {
        (Some(p), None) => format!("usb_path:{p}"),
        (None, Some(id)) => format!("by_id:{id}"),
        _ => {
            errors.push(format!("{at}: set exactly one of \"usb_path\" or \"by_id\""));
            return;
        }
    };
    let value = usb_path.or(by_id).unwrap_or_default();
    if value.is_empty() || value.contains(char::is_whitespace) || value.contains('/') {
        errors.push(format!("{at}: invalid identifier \"{value}\" (empty, whitespace or '/')"));
    } else if !seen.insert(key) {
        errors.push(format!("{at}: device \"{value}\" already described above"));
    }
}

fn check_name(at: &str, name: Option<&str>, seen: &mut HashSet<String>, errors: &mut Vec<String>) {
    let Some(name) = name else { return };
    if let Err(e) = validate_name(name) {
        errors.push(format!("{at}.name: {e}"));
    } else if !seen.insert(name.to_string()) {
        errors.push(format!("{at}.name: \"{name}\" already used (names must be unique)"));
    }
}

fn check_range(
    at: &str,
    field: &str,
    value: Option<u32>,
    range: std::ops::RangeInclusive<u32>,
    errors: &mut Vec<String>,
) {
    if let Some(v) = value
        && !range.contains(&v)
    {
        errors.push(format!("{at}.{field}: {v} out of bounds {}..={}", range.start(), range.end()));
    }
}

/// Content written on creation: no overrides, but the format documented for manual editing.
const TEMPLATE: &str = "\
# RaceCast — per-device overrides (docs/SPEC.md §8).
# Empty = every detected device uses the .env defaults.
# Written by the program (admin page). After a manual edit, run
# \"systemctl reload racecast\"; an invalid version is rejected and the previous one stays active.
#
# cameras:
#   - usb_path: platform-3610000.usb-usb-0:2.1   # physical USB port, or by_id: usb-HD_USB_Camera_HD_USB_Camera
#     name: cam-front                            # a-z 0-9 - _ (files, logs, LiveKit tracks)
#     rotation: 180                              # 0, 90, 180 or 270 (clockwise)
#     main: true                                 # main camera (at most one)
#     video_bitrate_kbps: 12000
#     stream:
#       enabled: true
#       resolution: 1280x720
#       fps: 30
#       bitrate_kbps: 2500
# microphones:
#   - by_id: usb-C-Media_Electronics_Inc._USB_Audio_Device-00
#     name: mic-driver
#     channels: 1
#     bit_depth: 16
#     stream:
#       enabled: true
#       bitrate_kbps: 64
";

/// A valid `devices.yml` read from disk, not applied yet: it becomes the active configuration only once
/// the devices it changes have been confirmed (SPEC §8).
pub struct Candidate {
    pub file: Arc<DevicesFile>,
    text: String,
}

/// Confirmed per-device configuration (the one devices run with) and the files that persist it.
pub struct DevicesStore {
    path: PathBuf,
    last_good: PathBuf,
    rejected: PathBuf,
    current: Mutex<Arc<DevicesFile>>,
    /// `devices.yml` differed from the last confirmed version at startup (edited while the program was
    /// stopped, or a change interrupted by a restart): it must go through a confirmation.
    pending_at_startup: bool,
}

impl DevicesStore {
    /// Loads the configuration (file created empty if missing). A file that differs from the last confirmed
    /// version is not trusted yet: the program starts with the confirmed one and applies the file with a
    /// confirmation ([`Self::pending_at_startup`]). Invalid file → last confirmed version, otherwise no
    /// overrides. Never returns an error.
    pub fn open(path: PathBuf) -> Self {
        let sibling = |suffix: &str| {
            let mut name = path.file_name().unwrap_or_default().to_os_string();
            name.push(suffix);
            path.with_file_name(name)
        };
        let (last_good, rejected) = (sibling(".last-good"), sibling(".rejected"));
        let confirmed = fs::read_to_string(&last_good).ok().and_then(|t| DevicesFile::parse(&last_good, &t).ok());

        let mut pending_at_startup = false;
        let initial = match fs::read_to_string(&path) {
            Err(e) if e.kind() == io::ErrorKind::NotFound => {
                match storage::write_atomic(&path, TEMPLATE.as_bytes()) {
                    Ok(()) => tracing::info!(path = %path.display(), "devices.yml created (no overrides)"),
                    Err(e) => tracing::error!(path = %path.display(), error = %e, "cannot create devices.yml"),
                }
                DevicesFile::default()
            }
            Err(source) => fallback(&last_good, &DevicesError::Read { path: path.clone(), source }),
            Ok(text) => match DevicesFile::parse(&path, &text) {
                Ok(file) => match confirmed {
                    Some(confirmed) if confirmed != file => {
                        tracing::info!(
                            "devices.yml differs from its last confirmed version: starting with the confirmed one, \
                             then applying the file with a confirmation"
                        );
                        pending_at_startup = true;
                        confirmed
                    }
                    _ => {
                        save_last_good(&last_good, &text);
                        file
                    }
                },
                Err(e) => fallback(&last_good, &e),
            },
        };
        tracing::info!(
            cameras = initial.cameras.len(),
            microphones = initial.microphones.len(),
            "device overrides loaded"
        );
        Self { path, last_good, rejected, current: Mutex::new(Arc::new(initial)), pending_at_startup }
    }

    fn lock(&self) -> std::sync::MutexGuard<'_, Arc<DevicesFile>> {
        self.current.lock().unwrap_or_else(std::sync::PoisonError::into_inner)
    }

    /// The confirmed configuration.
    pub fn current(&self) -> Arc<DevicesFile> {
        self.lock().clone()
    }

    pub fn pending_at_startup(&self) -> bool {
        self.pending_at_startup
    }

    /// Reads and validates the file. `None` if it matches the confirmed configuration.
    pub fn read(&self) -> Result<Option<Candidate>, DevicesError> {
        let text =
            fs::read_to_string(&self.path).map_err(|source| DevicesError::Read { path: self.path.clone(), source })?;
        let file = DevicesFile::parse(&self.path, &text)?;
        if **self.lock() == file {
            save_last_good(&self.last_good, &text); // comments may have changed
            return Ok(None);
        }
        Ok(Some(Candidate { file: Arc::new(file), text }))
    }

    /// The candidate's devices are confirmed: it becomes the active and last valid configuration.
    pub fn commit(&self, candidate: Candidate) {
        save_last_good(&self.last_good, &candidate.text);
        *self.lock() = candidate.file;
    }

    /// The candidate was rolled back: `devices.yml` gets the confirmed configuration back (so that a restart
    /// does not apply the rejected one again), the rejected content is kept in `devices.yml.rejected`. The
    /// file is left alone if it was edited again in the meantime.
    pub fn reject(&self, candidate: &Candidate) -> Result<String, String> {
        storage::write_atomic(&self.rejected, candidate.text.as_bytes())
            .map_err(|e| format!("cannot save {}: {e}", self.rejected.display()))?;
        if fs::read_to_string(&self.path).ok().as_deref() != Some(candidate.text.as_str()) {
            return Ok(format!("{} changed again meanwhile, left as is", self.path.display()));
        }
        let current = self.current();
        let restored = fs::read_to_string(&self.last_good)
            .ok()
            .filter(|t| DevicesFile::parse(&self.last_good, t).is_ok_and(|f| f == *current))
            .or_else(|| (*current == DevicesFile::default()).then(|| TEMPLATE.to_string()))
            .or_else(|| serde_yaml_ng::to_string(&*current).ok())
            .ok_or("cannot serialize the confirmed configuration")?;
        storage::write_atomic(&self.path, restored.as_bytes())
            .map_err(|e| format!("cannot restore {}: {e}", self.path.display()))?;
        Ok(format!("{} restored, rejected version kept in {}", self.path.display(), self.rejected.display()))
    }
}

fn fallback(last_good: &Path, error: &DevicesError) -> DevicesFile {
    tracing::error!(error = %error, "devices.yml rejected");
    let restored = fs::read_to_string(last_good).ok().and_then(|t| DevicesFile::parse(last_good, &t).ok());
    match restored {
        Some(file) => {
            tracing::warn!(path = %last_good.display(), "using the last valid version of devices.yml");
            file
        }
        None => {
            tracing::warn!("no valid version of devices.yml: .env defaults only");
            DevicesFile::default()
        }
    }
}

fn save_last_good(last_good: &Path, text: &str) {
    if fs::read_to_string(last_good).is_ok_and(|t| t == text) {
        return;
    }
    if let Err(e) = storage::write_atomic(last_good, text.as_bytes()) {
        tracing::warn!(path = %last_good.display(), error = %e, "cannot save the last valid version");
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    fn parse(text: &str) -> Result<DevicesFile, DevicesError> {
        DevicesFile::parse(Path::new("devices.yml"), text)
    }

    fn errors(text: &str) -> Vec<String> {
        match parse(text) {
            Err(DevicesError::Invalid { errors, .. }) => errors,
            other => panic!("expected validation errors, got {other:?}"),
        }
    }

    #[test]
    fn empty_and_template_mean_no_override() {
        assert_eq!(parse("").unwrap(), DevicesFile::default());
        assert_eq!(parse("   \n").unwrap(), DevicesFile::default());
        assert_eq!(parse(TEMPLATE).unwrap(), DevicesFile::default());
    }

    #[test]
    fn uncommented_template_is_valid() {
        // The example starts at "# cameras:"; the lines before it are the explanatory header.
        let text: String = TEMPLATE
            .lines()
            .skip_while(|l| *l != "# cameras:")
            .filter_map(|l| l.strip_prefix("# "))
            .collect::<Vec<_>>()
            .join("\n");
        let file = parse(&text).unwrap();
        assert_eq!(file.cameras.len(), 1);
        let cam = &file.cameras[0];
        assert_eq!(cam.rotation, Some(Rotation::Cw180));
        assert!(cam.main);
        assert_eq!(cam.stream.as_ref().and_then(|s| s.resolution), Some(Resolution::new(1280, 720)));
        assert_eq!(file.microphones[0].name.as_deref(), Some("mic-driver"));
    }

    #[test]
    fn syntax_and_unknown_fields_rejected() {
        assert!(matches!(parse("cameras: ["), Err(DevicesError::Parse { .. })));
        assert!(matches!(parse("camera: []"), Err(DevicesError::Parse { .. })));
        assert!(matches!(parse("cameras:\n  - by_id: x\n    rotaton: 90\n"), Err(DevicesError::Parse { .. })));
        assert!(matches!(parse("cameras:\n  - by_id: x\n    rotation: 45\n"), Err(DevicesError::Parse { .. })));
        assert!(matches!(
            parse("cameras:\n  - by_id: x\n    stream: { resolution: 1281x720 }\n"),
            Err(DevicesError::Parse { .. })
        ));
    }

    #[test]
    fn semantic_rules() {
        let e = errors("cameras:\n  - name: a\n  - usb_path: p\n    by_id: q\n");
        assert_eq!(e.len(), 2, "{e:?}");

        let e = errors("cameras:\n  - by_id: x\n    main: true\n  - by_id: y\n    main: true\n");
        assert!(e[0].contains("main"), "{e:?}");

        let e = errors("cameras:\n  - by_id: x\n    name: cam\nmicrophones:\n  - by_id: m\n    name: cam\n");
        assert!(e[0].contains("already used"), "{e:?}");

        let e = errors("cameras:\n  - by_id: x\n  - by_id: x\n");
        assert!(e[0].contains("already described"), "{e:?}");

        let e = errors(
            "cameras:\n  - by_id: x\n    video_bitrate_kbps: 10\n    stream: { fps: 0, bitrate_kbps: 99999 }\n\
             microphones:\n  - by_id: m\n    channels: 0\n    bit_depth: 20\n    stream: { bitrate_kbps: 1 }\n",
        );
        assert_eq!(e.len(), 6, "{e:?}");

        let e = errors("cameras:\n  - by_id: \"a b\"\n  - usb_path: \"../x\"\n");
        assert_eq!(e.len(), 2, "{e:?}");
    }

    #[test]
    fn lookup_prefers_usb_port() {
        let file = parse(
            "cameras:\n  - by_id: cam-id\n    name: by-id\n  - usb_path: port-1\n    name: by-port\n\
             microphones:\n  - usb_path: port-1\n    name: mic\n",
        )
        .unwrap();
        let name = |c: Option<&CameraOverride>| c.and_then(|c| c.name.clone());
        assert_eq!(name(file.camera(Some("port-1"), Some("cam-id"))).as_deref(), Some("by-port"));
        assert_eq!(name(file.camera(Some("port-2"), Some("cam-id"))).as_deref(), Some("by-id"));
        assert_eq!(name(file.camera(Some("port-2"), None)), None);
        assert!(file.microphone(Some("port-1"), None).is_some());
        let mut reserved: Vec<_> = file.reserved_names().collect();
        reserved.sort();
        assert_eq!(reserved, ["by-id", "by-port", "mic"]);
    }

    #[test]
    fn camera_and_mic_may_share_an_identifier() {
        // A camera with a built-in microphone has the same USB port for both sources.
        let text = "cameras:\n  - usb_path: p\nmicrophones:\n  - usb_path: p\n";
        assert!(parse(text).is_ok());
    }
}
