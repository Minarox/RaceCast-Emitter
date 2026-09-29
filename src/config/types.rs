//! Value types shared by `.env` and `devices.yml`, with their validation bounds.

use std::fmt;
use std::str::FromStr;

use serde::{Deserialize, Serialize};

/// Accepted bounds. An out-of-bounds value is rejected (default value in `.env`, rejection in `devices.yml`).
pub mod limits {
    use std::ops::RangeInclusive;

    pub const DIMENSION: RangeInclusive<u32> = 16..=7680;
    pub const FPS: RangeInclusive<u32> = 1..=240;
    pub const VIDEO_BITRATE_KBPS: RangeInclusive<u32> = 500..=100_000;
    pub const STREAM_VIDEO_BITRATE_KBPS: RangeInclusive<u32> = 100..=20_000;
    /// Opus bitrate range.
    pub const STREAM_AUDIO_BITRATE_KBPS: RangeInclusive<u32> = 6..=510;
    pub const SAMPLE_RATE: RangeInclusive<u32> = 8_000..=192_000;
    pub const CHANNELS: RangeInclusive<u32> = 1..=8;
    pub const BIT_DEPTHS: [u32; 3] = [16, 24, 32];
    pub const INTERVAL_MS: RangeInclusive<u64> = 100..=3_600_000;
    pub const DISK_CRITICAL_GB: RangeInclusive<u64> = 1..=10_000;
    /// 7-bit I2C addresses outside the reserved ranges.
    pub const I2C_ADDRESS: RangeInclusive<u8> = 0x03..=0x77;
    pub const NAME_MAX_LEN: usize = 32;
}

/// `WIDTHxHEIGHT` resolution (e.g. `1920x1080`).
#[derive(Debug, Clone, Copy, PartialEq, Eq, Serialize, Deserialize)]
#[serde(try_from = "String", into = "String")]
pub struct Resolution {
    pub width: u32,
    pub height: u32,
}

impl Resolution {
    pub const fn new(width: u32, height: u32) -> Self {
        Self { width, height }
    }
}

impl fmt::Display for Resolution {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        write!(f, "{}x{}", self.width, self.height)
    }
}

impl FromStr for Resolution {
    type Err = String;

    fn from_str(s: &str) -> Result<Self, Self::Err> {
        let (w, h) = s.trim().split_once(['x', 'X']).ok_or_else(|| format!("\"{s}\" is not WIDTHxHEIGHT"))?;
        let parse = |v: &str| v.trim().parse::<u32>().map_err(|_| format!("\"{s}\" is not WIDTHxHEIGHT"));
        let (width, height) = (parse(w)?, parse(h)?);
        for v in [width, height] {
            if !limits::DIMENSION.contains(&v) {
                return Err(format!(
                    "dimension {v} out of bounds ({}..={})",
                    limits::DIMENSION.start(),
                    limits::DIMENSION.end()
                ));
            }
            if v % 2 != 0 {
                return Err(format!("dimension {v} is odd (encoders require even dimensions)"));
            }
        }
        Ok(Self { width, height })
    }
}

impl TryFrom<String> for Resolution {
    type Error = String;

    fn try_from(s: String) -> Result<Self, Self::Error> {
        s.parse()
    }
}

impl From<Resolution> for String {
    fn from(r: Resolution) -> Self {
        r.to_string()
    }
}

/// Rotation applied right at capture, in degrees clockwise.
#[derive(Debug, Clone, Copy, PartialEq, Eq, Default, Serialize, Deserialize)]
#[serde(try_from = "u16", into = "u16")]
pub enum Rotation {
    #[default]
    None,
    Cw90,
    Cw180,
    Cw270,
}

impl Rotation {
    /// `flip-method` value of `nvvidconv` (0 none, 1 counterclockwise 90°, 2 180°, 3 clockwise 90°).
    pub fn nvvidconv_flip_method(self) -> u32 {
        match self {
            Self::None => 0,
            Self::Cw90 => 3,
            Self::Cw180 => 2,
            Self::Cw270 => 1,
        }
    }

    /// A quarter turn swaps width and height.
    pub fn swaps_dimensions(self) -> bool {
        matches!(self, Self::Cw90 | Self::Cw270)
    }
}

impl TryFrom<u16> for Rotation {
    type Error = String;

    fn try_from(deg: u16) -> Result<Self, Self::Error> {
        match deg {
            0 => Ok(Self::None),
            90 => Ok(Self::Cw90),
            180 => Ok(Self::Cw180),
            270 => Ok(Self::Cw270),
            _ => Err(format!("invalid rotation {deg} (0, 90, 180 or 270)")),
        }
    }
}

impl From<Rotation> for u16 {
    fn from(r: Rotation) -> Self {
        match r {
            Rotation::None => 0,
            Rotation::Cw90 => 90,
            Rotation::Cw180 => 180,
            Rotation::Cw270 => 270,
        }
    }
}

/// Valid device name: used as-is in file names, logs and LiveKit track names.
pub fn validate_name(name: &str) -> Result<(), String> {
    let mut chars = name.chars();
    let first_ok = chars.next().is_some_and(|c| c.is_ascii_lowercase() || c.is_ascii_digit());
    let rest_ok = chars.all(|c| c.is_ascii_lowercase() || c.is_ascii_digit() || c == '-' || c == '_');
    if name.len() > limits::NAME_MAX_LEN || !first_ok || !rest_ok {
        return Err(format!(
            "invalid name \"{name}\" (1 to {} characters among a-z, 0-9, '-', '_', starting with a letter or \
             a digit)",
            limits::NAME_MAX_LEN
        ));
    }
    Ok(())
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn resolution_parse() {
        assert_eq!("1920x1080".parse(), Ok(Resolution::new(1920, 1080)));
        assert_eq!(" 960X540 ".parse(), Ok(Resolution::new(960, 540)));
        assert!("1920".parse::<Resolution>().is_err());
        assert!("1920x".parse::<Resolution>().is_err());
        assert!("axb".parse::<Resolution>().is_err());
        assert!("8x8".parse::<Resolution>().is_err());
        assert!("961x540".parse::<Resolution>().is_err());
        assert_eq!(Resolution::new(1280, 720).to_string(), "1280x720");
    }

    #[test]
    fn rotation_values() {
        assert_eq!(Rotation::try_from(90), Ok(Rotation::Cw90));
        assert!(Rotation::try_from(45).is_err());
        assert_eq!(u16::from(Rotation::Cw270), 270);
    }

    #[test]
    fn names() {
        assert!(validate_name("cam-front").is_ok());
        assert!(validate_name("mic_2").is_ok());
        assert!(validate_name("").is_err());
        assert!(validate_name("-cam").is_err());
        assert!(validate_name("Cam").is_err());
        assert!(validate_name("cam front").is_err());
        assert!(validate_name("cam/../x").is_err());
        assert!(validate_name(&"a".repeat(33)).is_err());
    }
}
