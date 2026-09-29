//! Global settings read from `.env` (and from the process environment, which takes precedence).
//!
//! No error is fatal: a missing value takes the built-in default, and so does an invalid one, with a
//! warning. Loading happens before logging is initialized (logging depends on `LOG_DIR`), so warnings
//! are collected and logged by the caller.

use std::collections::HashMap;
use std::fmt;
use std::ops::RangeInclusive;
use std::path::{Path, PathBuf};
use std::str::FromStr;
use std::time::Duration;

use tracing_subscriber::EnvFilter;

use super::paths;
use super::types::{Resolution, limits};

#[derive(Debug, Clone)]
pub struct Settings {
    pub env_file: PathBuf,
    /// Directory of the `.env` file: base for relative paths.
    pub base_dir: PathBuf,
    /// `None` if the URL or keys are missing: streaming is then disabled, recording is not.
    pub livekit: Option<LiveKitSettings>,
    pub recordings_dir: PathBuf,
    pub log_dir: PathBuf,
    pub devices_file: PathBuf,
    pub disk_critical_gb: u64,
    pub log_filter: String,
    pub video: VideoDefaults,
    pub audio: AudioDefaults,
    pub telemetry: TelemetrySettings,
}

#[derive(Clone)]
pub struct LiveKitSettings {
    pub url: String,
    pub api_key: String,
    pub api_secret: String,
    pub room: String,
    pub identity: String,
}

/// Credentials in logs: the key only as a short prefix, the secret never.
fn mask(key: &str) -> String {
    format!("{}…", key.chars().take(4).collect::<String>())
}

impl fmt::Debug for LiveKitSettings {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        f.debug_struct("LiveKitSettings")
            .field("url", &self.url)
            .field("api_key", &mask(&self.api_key))
            .field("api_secret", &"***")
            .field("room", &self.room)
            .field("identity", &self.identity)
            .finish()
    }
}

#[derive(Debug, Clone)]
pub struct VideoDefaults {
    pub max_resolution: Resolution,
    pub max_fps: u32,
    pub bitrate_kbps: u32,
    pub stream: StreamVideoDefaults,
}

#[derive(Debug, Clone)]
pub struct StreamVideoDefaults {
    pub enabled: bool,
    pub resolution: Resolution,
    pub fps: u32,
    pub bitrate_kbps: u32,
}

#[derive(Debug, Clone)]
pub struct AudioDefaults {
    pub sample_rate: u32,
    pub stream_enabled: bool,
    pub stream_bitrate_kbps: u32,
}

#[derive(Debug, Clone)]
pub struct TelemetrySettings {
    pub gps_interval: Duration,
    pub modem_interval: Duration,
    pub ups_interval: Duration,
    pub system_interval: Duration,
    pub ups_i2c_bus: u8,
    pub ups_i2c_address: u8,
}

/// Raw values: the `.env` file merged with the process environment (which takes precedence).
pub(super) struct EnvSource {
    vars: HashMap<String, String>,
}

impl EnvSource {
    /// Reads the file, then overlays the environment. Missing file or invalid line → warning.
    pub(super) fn load(path: &Path, warnings: &mut Vec<String>) -> Self {
        let mut vars = HashMap::new();
        match dotenvy::from_path_iter(path) {
            Ok(iter) => {
                for item in iter {
                    match item {
                        Ok((k, v)) => {
                            vars.insert(k, v);
                        }
                        Err(e) => {
                            warnings.push(format!("{}: reading stopped ({e})", path.display()));
                            break;
                        }
                    }
                }
            }
            Err(e) => warnings
                .push(format!("{} unreadable ({e}): built-in defaults used, streaming disabled", path.display())),
        }
        // `vars_os`: `vars()` would panic on a non-UTF-8 variable.
        for (k, v) in std::env::vars_os() {
            if let (Ok(k), Ok(v)) = (k.into_string(), v.into_string()) {
                vars.insert(k, v);
            }
        }
        Self { vars }
    }

    #[cfg(test)]
    pub(super) fn from_pairs(pairs: &[(&str, &str)]) -> Self {
        Self { vars: pairs.iter().map(|(k, v)| (k.to_string(), v.to_string())).collect() }
    }

    /// Non-empty value (a key that is present but empty, e.g. `LIVEKIT_API_KEY=`, counts as missing).
    fn get(&self, key: &str) -> Option<&str> {
        self.vars.get(key).map(|v| v.trim()).filter(|v| !v.is_empty())
    }
}

/// Typed reader that falls back to the default on error, recording a warning.
struct Reader<'a> {
    src: &'a EnvSource,
    warnings: &'a mut Vec<String>,
}

impl Reader<'_> {
    fn parse<T, E>(&mut self, key: &str, default: T, check: impl Fn(&T) -> Result<(), String>) -> T
    where
        T: FromStr<Err = E> + fmt::Display,
        E: fmt::Display,
    {
        let Some(raw) = self.src.get(key) else { return default };
        let result = raw.parse::<T>().map_err(|e| e.to_string()).and_then(|v| check(&v).map(|()| v));
        match result {
            Ok(v) => v,
            Err(reason) => {
                self.warnings.push(format!("{key}={raw:?} is invalid ({reason}): default value {default} used"));
                default
            }
        }
    }

    fn u32_in(&mut self, key: &str, default: u32, range: RangeInclusive<u32>) -> u32 {
        self.parse(key, default, |v| in_range(v, &range))
    }

    fn millis(&mut self, key: &str, default_ms: u64) -> Duration {
        Duration::from_millis(self.parse(key, default_ms, |v| in_range(v, &limits::INTERVAL_MS)))
    }

    fn bool(&mut self, key: &str, default: bool) -> bool {
        let Some(raw) = self.src.get(key) else { return default };
        match raw.to_ascii_lowercase().as_str() {
            "true" | "1" | "yes" | "on" => true,
            "false" | "0" | "no" | "off" => false,
            _ => {
                self.warnings
                    .push(format!("{key}={raw:?} is invalid (true or false expected): default value {default} used"));
                default
            }
        }
    }

    fn string(&self, key: &str, default: &str) -> String {
        self.src.get(key).unwrap_or(default).to_string()
    }

    fn path(&self, key: &str, default: &str, base: &Path, home: Option<&Path>) -> PathBuf {
        paths::resolve(base, home, self.src.get(key).unwrap_or(default))
    }
}

fn in_range<T: PartialOrd + fmt::Display>(v: &T, range: &RangeInclusive<T>) -> Result<(), String> {
    if range.contains(v) { Ok(()) } else { Err(format!("out of bounds {}..={}", range.start(), range.end())) }
}

/// I2C address in decimal or hexadecimal (`0x41`).
#[derive(Debug, Clone, Copy)]
struct I2cAddress(u8);

impl FromStr for I2cAddress {
    type Err = String;

    fn from_str(s: &str) -> Result<Self, Self::Err> {
        let parsed = match s.strip_prefix("0x").or_else(|| s.strip_prefix("0X")) {
            Some(hex) => u8::from_str_radix(hex, 16),
            None => s.parse(),
        };
        parsed.map(Self).map_err(|_| format!("\"{s}\" is not an address"))
    }
}

impl fmt::Display for I2cAddress {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        write!(f, "{:#04x}", self.0)
    }
}

impl Settings {
    /// Loads `env_file`. Never panics and never returns an error: every problem becomes a warning in
    /// `warnings` and the corresponding default value is used.
    pub fn load(env_file: &Path, warnings: &mut Vec<String>) -> Self {
        let src = EnvSource::load(env_file, warnings);
        let home = std::env::var_os("HOME").map(PathBuf::from);
        Self::from_source(env_file, &src, home.as_deref(), warnings)
    }

    pub(super) fn from_source(
        env_file: &Path,
        src: &EnvSource,
        home: Option<&Path>,
        warnings: &mut Vec<String>,
    ) -> Self {
        let env_file = std::path::absolute(env_file).unwrap_or_else(|_| env_file.to_path_buf());
        let base_dir = env_file.parent().map(Path::to_path_buf).unwrap_or_else(|| PathBuf::from("/"));
        let mut r = Reader { src, warnings };

        let livekit = livekit(&mut r);
        let recordings_dir = r.path("RECORDINGS_DIR", "./records", &base_dir, home);
        let log_dir = r.path("LOG_DIR", "./logs", &base_dir, home);
        let devices_file = r.path("DEVICES_FILE", "./devices.yml", &base_dir, home);
        let disk_critical_gb = r.parse("DISK_CRITICAL_GB", 5, |v| in_range(v, &limits::DISK_CRITICAL_GB));

        let mut log_filter = r.string("RUST_LOG", "info");
        if let Err(e) = EnvFilter::try_new(&log_filter) {
            r.warnings.push(format!("RUST_LOG={log_filter:?} is invalid ({e}): \"info\" used"));
            log_filter = "info".into();
        }

        let video = VideoDefaults {
            max_resolution: r.parse("VIDEO_MAX_RESOLUTION", Resolution::new(1920, 1080), |_| Ok(())),
            max_fps: r.u32_in("VIDEO_MAX_FPS", 60, limits::FPS),
            bitrate_kbps: r.u32_in("VIDEO_BITRATE_KBPS", 12_000, limits::VIDEO_BITRATE_KBPS),
            stream: StreamVideoDefaults {
                enabled: r.bool("STREAM_VIDEO_ENABLED", true),
                resolution: r.parse("STREAM_VIDEO_RESOLUTION", Resolution::new(960, 540), |_| Ok(())),
                fps: r.u32_in("STREAM_VIDEO_FPS", 30, limits::FPS),
                bitrate_kbps: r.u32_in("STREAM_VIDEO_BITRATE_KBPS", 1_200, limits::STREAM_VIDEO_BITRATE_KBPS),
            },
        };
        let audio = AudioDefaults {
            sample_rate: r.u32_in("AUDIO_SAMPLE_RATE", 48_000, limits::SAMPLE_RATE),
            stream_enabled: r.bool("STREAM_AUDIO_ENABLED", true),
            stream_bitrate_kbps: r.u32_in("STREAM_AUDIO_BITRATE_KBPS", 64, limits::STREAM_AUDIO_BITRATE_KBPS),
        };
        let telemetry = TelemetrySettings {
            gps_interval: r.millis("GPS_INTERVAL_MS", 1_000),
            modem_interval: r.millis("MODEM_INTERVAL_MS", 5_000),
            ups_interval: r.millis("UPS_INTERVAL_MS", 2_000),
            system_interval: r.millis("SYSTEM_INTERVAL_MS", 5_000),
            ups_i2c_bus: r.parse("UPS_I2C_BUS", 7u8, |_| Ok(())),
            ups_i2c_address: r.parse("UPS_I2C_ADDRESS", I2cAddress(0x41), |a| in_range(&a.0, &limits::I2C_ADDRESS)).0,
        };

        Self {
            env_file,
            base_dir,
            livekit,
            recordings_dir,
            log_dir,
            devices_file,
            disk_critical_gb,
            log_filter,
            video,
            audio,
            telemetry,
        }
    }

    /// Logs the effective configuration (resolved paths included), without secrets.
    pub fn log_summary(&self) {
        tracing::info!(
            env_file = %self.env_file.display(),
            base_dir = %self.base_dir.display(),
            recordings_dir = %self.recordings_dir.display(),
            log_dir = %self.log_dir.display(),
            devices_file = %self.devices_file.display(),
            disk_critical_gb = self.disk_critical_gb,
            log_filter = %self.log_filter,
            "config: paths"
        );
        match &self.livekit {
            Some(lk) => tracing::info!(
                url = %lk.url,
                room = %lk.room,
                identity = %lk.identity,
                api_key = %mask(&lk.api_key),
                api_secret_set = !lk.api_secret.is_empty(),
                "config: LiveKit"
            ),
            None => tracing::warn!("config: LiveKit incomplete, streaming disabled"),
        }
        let (v, a, t) = (&self.video, &self.audio, &self.telemetry);
        tracing::info!(
            max_resolution = %v.max_resolution,
            max_fps = v.max_fps,
            bitrate_kbps = v.bitrate_kbps,
            stream_enabled = v.stream.enabled,
            stream_resolution = %v.stream.resolution,
            stream_fps = v.stream.fps,
            stream_bitrate_kbps = v.stream.bitrate_kbps,
            "config: video defaults"
        );
        tracing::info!(
            sample_rate = a.sample_rate,
            stream_enabled = a.stream_enabled,
            stream_bitrate_kbps = a.stream_bitrate_kbps,
            "config: audio defaults"
        );
        tracing::info!(
            gps_ms = t.gps_interval.as_millis() as u64,
            modem_ms = t.modem_interval.as_millis() as u64,
            ups_ms = t.ups_interval.as_millis() as u64,
            system_ms = t.system_interval.as_millis() as u64,
            ups_i2c_bus = t.ups_i2c_bus,
            ups_i2c_address = %format!("{:#04x}", t.ups_i2c_address),
            "config: telemetry"
        );
    }
}

fn livekit(r: &mut Reader<'_>) -> Option<LiveKitSettings> {
    let url = r.src.get("LIVEKIT_URL");
    let key = r.src.get("LIVEKIT_API_KEY");
    let secret = r.src.get("LIVEKIT_API_SECRET");
    let (Some(url), Some(api_key), Some(api_secret)) = (url, key, secret) else {
        let missing: Vec<&str> = [("LIVEKIT_URL", url), ("LIVEKIT_API_KEY", key), ("LIVEKIT_API_SECRET", secret)]
            .into_iter()
            .filter_map(|(k, v)| v.is_none().then_some(k))
            .collect();
        r.warnings.push(format!("{} missing: LiveKit streaming disabled", missing.join(", ")));
        return None;
    };
    if !(url.starts_with("wss://") || url.starts_with("ws://")) {
        r.warnings.push(format!("LIVEKIT_URL={url:?} is invalid (ws:// or wss:// expected): streaming disabled"));
        return None;
    }
    Some(LiveKitSettings {
        url: url.to_string(),
        api_key: api_key.to_string(),
        api_secret: api_secret.to_string(),
        room: r.string("LIVEKIT_ROOM", "racecast"),
        identity: r.string("LIVEKIT_IDENTITY", "racecast-car"),
    })
}

#[cfg(test)]
mod tests {
    use super::*;

    fn load(pairs: &[(&str, &str)]) -> (Settings, Vec<String>) {
        let mut warnings = Vec::new();
        let src = EnvSource::from_pairs(pairs);
        let s = Settings::from_source(
            Path::new("/opt/racecast/.env"),
            &src,
            Some(Path::new("/home/jetson")),
            &mut warnings,
        );
        (s, warnings)
    }

    const LK: [(&str, &str); 3] =
        [("LIVEKIT_URL", "wss://live.example.org"), ("LIVEKIT_API_KEY", "key"), ("LIVEKIT_API_SECRET", "s3cr3t")];

    #[test]
    fn defaults_when_empty() {
        let (s, w) = load(&LK);
        assert!(w.is_empty(), "{w:?}");
        assert_eq!(s.base_dir, PathBuf::from("/opt/racecast"));
        assert_eq!(s.recordings_dir, PathBuf::from("/opt/racecast/records"));
        assert_eq!(s.devices_file, PathBuf::from("/opt/racecast/devices.yml"));
        assert_eq!(s.disk_critical_gb, 5);
        assert_eq!(s.video.max_resolution, Resolution::new(1920, 1080));
        assert_eq!(s.video.stream.bitrate_kbps, 1_200);
        assert_eq!(s.telemetry.ups_i2c_address, 0x41);
        assert_eq!(s.telemetry.gps_interval, Duration::from_secs(1));
        let lk = s.livekit.expect("livekit");
        assert_eq!((lk.room.as_str(), lk.identity.as_str()), ("racecast", "racecast-car"));
    }

    #[test]
    fn invalid_values_fall_back_with_warning() {
        let mut pairs = LK.to_vec();
        pairs.extend([
            ("VIDEO_MAX_FPS", "abc"),
            ("VIDEO_BITRATE_KBPS", "50"),
            ("STREAM_VIDEO_RESOLUTION", "961x540"),
            ("STREAM_AUDIO_ENABLED", "maybe"),
            ("UPS_I2C_ADDRESS", "0x80"),
            ("RUST_LOG", "info,,=="),
            ("GPS_INTERVAL_MS", "10"),
        ]);
        let (s, w) = load(&pairs);
        assert_eq!(w.len(), 7, "{w:#?}");
        assert_eq!(s.video.max_fps, 60);
        assert_eq!(s.video.bitrate_kbps, 12_000);
        assert_eq!(s.video.stream.resolution, Resolution::new(960, 540));
        assert!(s.audio.stream_enabled);
        assert_eq!(s.telemetry.ups_i2c_address, 0x41);
        assert_eq!(s.log_filter, "info");
        assert_eq!(s.telemetry.gps_interval, Duration::from_secs(1));
    }

    #[test]
    fn valid_values_applied() {
        let mut pairs = LK.to_vec();
        pairs.extend([
            ("UPS_I2C_ADDRESS", "65"),
            ("STREAM_VIDEO_ENABLED", "false"),
            ("RECORDINGS_DIR", "~/rec"),
            ("LOG_DIR", "/var/log/racecast"),
            ("LIVEKIT_ROOM", "race"),
        ]);
        let (s, w) = load(&pairs);
        assert!(w.is_empty(), "{w:?}");
        assert_eq!(s.telemetry.ups_i2c_address, 0x41);
        assert!(!s.video.stream.enabled);
        assert_eq!(s.recordings_dir, PathBuf::from("/home/jetson/rec"));
        assert_eq!(s.log_dir, PathBuf::from("/var/log/racecast"));
        assert_eq!(s.livekit.map(|l| l.room), Some("race".into()));
    }

    #[test]
    fn livekit_disabled_when_incomplete() {
        let (s, w) = load(&[("LIVEKIT_URL", "wss://x"), ("LIVEKIT_API_KEY", "k"), ("LIVEKIT_API_SECRET", " ")]);
        assert!(s.livekit.is_none());
        assert!(w[0].contains("LIVEKIT_API_SECRET"), "{w:?}");
        let (s, _) = load(&[("LIVEKIT_URL", "https://x"), ("LIVEKIT_API_KEY", "k"), ("LIVEKIT_API_SECRET", "s")]);
        assert!(s.livekit.is_none());
    }

    #[test]
    fn secret_never_printed() {
        let (s, _) = load(&LK);
        assert!(!format!("{s:?}").contains("s3cr3t"));
        assert!(format!("{s:?}").contains("\"key…\""));
    }
}
