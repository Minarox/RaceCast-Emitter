//! Capture modes offered by a device (from its GStreamer caps) and selection of the mode to use.

use gstreamer as gst;

use crate::config::Resolution;

/// An MJPEG mode of a camera.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub struct VideoMode {
    pub width: u32,
    pub height: u32,
    pub fps_n: i32,
    pub fps_d: i32,
}

impl VideoMode {
    pub fn fps(&self) -> f64 {
        f64::from(self.fps_n) / f64::from(self.fps_d.max(1))
    }

    /// Integer frame rate used for the timecode and the keyframe interval.
    pub fn fps_rounded(&self) -> u32 {
        self.fps().round().max(1.0) as u32
    }
}

impl std::fmt::Display for VideoMode {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        write!(f, "{}x{}@{}", self.width, self.height, self.fps_rounded())
    }
}

/// MJPEG modes found in `caps` (one entry per resolution and frame rate).
pub fn video_modes(caps: &gst::CapsRef) -> Vec<VideoMode> {
    let mut modes = Vec::new();
    for s in caps.iter().filter(|s| s.name() == "image/jpeg") {
        let (Ok(width), Ok(height)) = (s.get::<i32>("width"), s.get::<i32>("height")) else { continue };
        let (Ok(width), Ok(height)) = (u32::try_from(width), u32::try_from(height)) else { continue };
        for rate in fractions(s.value("framerate").ok()) {
            let mode = VideoMode { width, height, fps_n: rate.numer(), fps_d: rate.denom() };
            if rate.denom() > 0 && rate.numer() > 0 && !modes.contains(&mode) {
                modes.push(mode);
            }
        }
    }
    modes
}

fn fractions(value: Option<&gst::glib::SendValue>) -> Vec<gst::Fraction> {
    let Some(v) = value else { return Vec::new() };
    if let Ok(f) = v.get::<gst::Fraction>() {
        return vec![f];
    }
    if let Ok(list) = v.get::<gst::List>() {
        return list.iter().filter_map(|v| v.get::<gst::Fraction>().ok()).collect();
    }
    if let Ok(range) = v.get::<gst::FractionRange>() {
        return vec![range.max()];
    }
    Vec::new()
}

/// Candidate modes under the cap, best first: largest resolution, then highest frame rate. Stepping down the
/// list therefore lowers the frame rate first, then the resolution (fallback when USB bandwidth is short).
pub fn video_candidates(modes: &[VideoMode], max: Resolution, max_fps: u32) -> Vec<VideoMode> {
    let mut candidates: Vec<VideoMode> = modes
        .iter()
        .copied()
        .filter(|m| m.width <= max.width && m.height <= max.height && m.fps() <= f64::from(max_fps) + 0.01)
        .collect();
    candidates.sort_by(|a, b| {
        let area = |m: &VideoMode| u64::from(m.width) * u64::from(m.height);
        area(b).cmp(&area(a)).then(b.fps().total_cmp(&a.fps()))
    });
    candidates
}

/// Sample formats recorded as-is in a WAV file (little-endian, packed).
#[derive(Debug, Clone, Copy, PartialEq, Eq, PartialOrd, Ord)]
pub enum SampleFormat {
    S16,
    S24,
    S32,
}

impl SampleFormat {
    fn from_gst(name: &str) -> Option<Self> {
        match name {
            "S16LE" => Some(Self::S16),
            "S24LE" => Some(Self::S24),
            "S32LE" => Some(Self::S32),
            _ => None,
        }
    }

    pub fn gst_name(self) -> &'static str {
        match self {
            Self::S16 => "S16LE",
            Self::S24 => "S24LE",
            Self::S32 => "S32LE",
        }
    }

    pub fn bits(self) -> u16 {
        match self {
            Self::S16 => 16,
            Self::S24 => 24,
            Self::S32 => 32,
        }
    }
}

/// Values accepted for a caps field: an explicit list or an inclusive range.
#[derive(Debug, Clone, PartialEq)]
pub enum IntSet {
    List(Vec<u32>),
    Range(u32, u32),
}

impl IntSet {
    fn contains(&self, v: u32) -> bool {
        match self {
            Self::List(l) => l.contains(&v),
            Self::Range(min, max) => (*min..=*max).contains(&v),
        }
    }

    fn max(&self) -> Option<u32> {
        match self {
            Self::List(l) => l.iter().copied().max(),
            Self::Range(_, max) => Some(*max),
        }
    }
}

/// One `audio/x-raw` caps structure reduced to what matters for recording.
#[derive(Debug, Clone, PartialEq)]
pub struct AudioCapsEntry {
    pub formats: Vec<SampleFormat>,
    pub rates: IntSet,
    pub channels: IntSet,
}

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub struct AudioFormat {
    pub format: SampleFormat,
    pub rate: u32,
    pub channels: u32,
}

impl std::fmt::Display for AudioFormat {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        write!(f, "{} {} Hz {} ch", self.format.gst_name(), self.rate, self.channels)
    }
}

/// Interleaved `audio/x-raw` entries of `caps` with at least one directly recordable sample format.
pub fn audio_caps(caps: &gst::CapsRef) -> Vec<AudioCapsEntry> {
    caps.iter()
        .filter(|s| s.name() == "audio/x-raw")
        .filter(|s| s.get::<&str>("layout").map_or(true, |l| l == "interleaved"))
        .filter_map(|s| {
            let formats: Vec<SampleFormat> =
                strings(s.value("format").ok()).iter().filter_map(|f| SampleFormat::from_gst(f)).collect();
            let entry = AudioCapsEntry {
                formats,
                rates: int_set(s.value("rate").ok())?,
                channels: int_set(s.value("channels").ok())?,
            };
            (!entry.formats.is_empty()).then_some(entry)
        })
        .collect()
}

fn strings(value: Option<&gst::glib::SendValue>) -> Vec<String> {
    let Some(v) = value else { return Vec::new() };
    if let Ok(s) = v.get::<String>() {
        return vec![s];
    }
    if let Ok(list) = v.get::<gst::List>() {
        return list.iter().filter_map(|v| v.get::<String>().ok()).collect();
    }
    Vec::new()
}

fn int_set(value: Option<&gst::glib::SendValue>) -> Option<IntSet> {
    let v = value?;
    let positive = |i: i32| u32::try_from(i).ok();
    if let Ok(i) = v.get::<i32>() {
        return Some(IntSet::List(vec![positive(i)?]));
    }
    if let Ok(r) = v.get::<gst::IntRange<i32>>() {
        return Some(IntSet::Range(positive(r.min())?, positive(r.max())?));
    }
    if let Ok(list) = v.get::<gst::List>() {
        return Some(IntSet::List(list.iter().filter_map(|v| v.get::<i32>().ok().and_then(positive)).collect()));
    }
    None
}

/// Recording formats to try, best first. ALSA caps are approximate (a device may announce 24-bit mono and
/// refuse it), so the caller tries them in order until one starts. Order: requested bit depth, then the
/// highest; requested channel count, then the lowest (microphones are mono sources); for each, the preferred
/// rate if supported, otherwise the highest.
pub fn audio_candidates(
    entries: &[AudioCapsEntry],
    preferred_rate: u32,
    want_bits: Option<u32>,
    want_channels: Option<u32>,
) -> Vec<AudioFormat> {
    let mut out = Vec::new();
    for e in entries {
        let channels: Vec<u32> = match &e.channels {
            IntSet::List(l) => l.clone(),
            IntSet::Range(min, max) => (*min..=(*max).min(8)).collect(),
        };
        let rate = if e.rates.contains(preferred_rate) { Some(preferred_rate) } else { e.rates.max() };
        let Some(rate) = rate else { continue };
        for &format in &e.formats {
            for &ch in &channels {
                let f = AudioFormat { format, rate, channels: ch };
                if ch > 0 && !out.contains(&f) {
                    out.push(f);
                }
            }
        }
    }
    let rank = |f: &AudioFormat| {
        (
            want_bits != Some(u32::from(f.format.bits())),
            std::cmp::Reverse(f.format),
            want_channels != Some(f.channels),
            f.channels,
            f.rate != preferred_rate,
            std::cmp::Reverse(f.rate),
        )
    };
    out.sort_by_key(rank);
    out
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::str::FromStr;

    fn caps(s: &str) -> gst::Caps {
        gst::init().unwrap();
        gst::Caps::from_str(s).unwrap()
    }

    #[test]
    fn parses_uvc_mjpeg_caps() {
        let c = caps(
            "image/jpeg, width=1920, height=1080, framerate={ (fraction)30/1, (fraction)15/1 }; \
             image/jpeg, width=1280, height=720, framerate=60/1; video/x-raw, format=YUY2, width=640, height=480",
        );
        let modes = video_modes(&c);
        assert_eq!(modes.len(), 3);
        assert_eq!(modes[0], VideoMode { width: 1920, height: 1080, fps_n: 30, fps_d: 1 });
        assert_eq!(modes[2].to_string(), "1280x720@60");
    }

    #[test]
    fn selection_prefers_resolution_then_fps() {
        let m = |w, h, f| VideoMode { width: w, height: h, fps_n: f, fps_d: 1 };
        let modes = [m(1280, 720, 60), m(1920, 1080, 30), m(1920, 1080, 15), m(3840, 2160, 30), m(640, 480, 120)];
        let c = video_candidates(&modes, Resolution::new(1920, 1080), 60);
        assert_eq!(c, [m(1920, 1080, 30), m(1920, 1080, 15), m(1280, 720, 60)]);
        assert!(video_candidates(&modes, Resolution::new(320, 240), 60).is_empty());
        // Non-integer rates are compared as real numbers.
        let ntsc = VideoMode { width: 1920, height: 1080, fps_n: 30000, fps_d: 1001 };
        assert_eq!(video_candidates(&[ntsc], Resolution::new(1920, 1080), 30), [ntsc]);
        assert_eq!(ntsc.fps_rounded(), 30);
    }

    #[test]
    fn parses_alsa_caps() {
        let c = caps(
            "audio/x-raw, format={ (string)S24LE, (string)S16LE }, layout=interleaved, rate=[ 22050, 96000 ], channels=2; \
             audio/x-raw, format=S16LE, layout=interleaved, rate=48000, channels=[ 1, 2 ]; \
             audio/x-raw, format=S24_32LE, layout=interleaved, rate=48000, channels=2",
        );
        let e = audio_caps(&c);
        assert_eq!(e.len(), 2, "{e:?}");
        assert_eq!(e[0].formats, [SampleFormat::S24, SampleFormat::S16]);
        assert_eq!(e[0].rates, IntSet::Range(22050, 96000));
        assert_eq!(e[1].channels, IntSet::Range(1, 2));
    }

    #[test]
    fn audio_candidate_order() {
        let fmt = |format, rate, channels| AudioFormat { format, rate, channels };
        // Camera microphone as announced by alsasrc: one merged structure.
        let camera_mic = [AudioCapsEntry {
            formats: vec![SampleFormat::S24, SampleFormat::S16],
            rates: IntSet::Range(22050, 96000),
            channels: IntSet::Range(1, 2),
        }];
        let c = audio_candidates(&camera_mic, 48000, None, None);
        assert_eq!(
            c,
            [
                fmt(SampleFormat::S24, 48000, 1),
                fmt(SampleFormat::S24, 48000, 2),
                fmt(SampleFormat::S16, 48000, 1),
                fmt(SampleFormat::S16, 48000, 2),
            ]
        );
        // Requested 16-bit stereo comes first, the rest stays as a fallback.
        let c = audio_candidates(&camera_mic, 48000, Some(16), Some(2));
        assert_eq!(c[0], fmt(SampleFormat::S16, 48000, 2));
        assert_eq!(c.len(), 4);
        // Preferred rate unavailable: highest supported.
        let usb = [AudioCapsEntry {
            formats: vec![SampleFormat::S16],
            rates: IntSet::List(vec![44100, 32000]),
            channels: IntSet::List(vec![1]),
        }];
        assert_eq!(audio_candidates(&usb, 48000, None, None), [fmt(SampleFormat::S16, 44100, 1)]);
        assert!(audio_candidates(&[], 48000, None, None).is_empty());
    }
}
