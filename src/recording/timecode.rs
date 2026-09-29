//! Capture time → time-of-day timecode (video) and samples since midnight (BWF). The capture time is the
//! pipeline's `base_time` + the buffer's running time on the `realtime` system clock, never the arrival time.

use chrono::{DateTime, Local, TimeZone, Timelike};
use gstreamer as gst;

/// Local time of a Unix timestamp in nanoseconds.
pub fn local_time(unix_ns: u64) -> DateTime<Local> {
    Local.timestamp_nanos(i64::try_from(unix_ns).unwrap_or(i64::MAX))
}

/// Unix time (ns) at which a buffer was captured, from its PTS and the segment it belongs to.
pub fn capture_unix_ns(
    base_time: gst::ClockTime,
    segment: Option<&gst::FormattedSegment<gst::ClockTime>>,
    pts: gst::ClockTime,
) -> u64 {
    let running = segment.and_then(|s| s.to_running_time(pts)).unwrap_or(pts);
    base_time.nseconds().saturating_add(running.nseconds())
}

/// `(hours, minutes, seconds, frames)` of `t` at `fps` frames per second.
pub fn timecode_fields<T: Timelike>(t: &T, fps: u32) -> (u32, u32, u32, u32) {
    // `nanosecond()` exceeds 10^9 during a leap second: clamp to the last frame.
    let nanos = u64::from(t.nanosecond().min(999_999_999));
    let frame = (nanos * u64::from(fps) / 1_000_000_000) as u32;
    (t.hour(), t.minute(), t.second().min(59), frame)
}

/// Samples elapsed since local midnight at `rate` Hz (BWF `TimeReference`).
pub fn samples_since_midnight<T: Timelike>(t: &T, rate: u32) -> u64 {
    let nanos = u64::from(t.nanosecond().min(999_999_999));
    u64::from(t.num_seconds_from_midnight()) * u64::from(rate) + nanos * u64::from(rate) / 1_000_000_000
}

#[cfg(test)]
mod tests {
    use super::*;
    use chrono::NaiveTime;

    fn at(h: u32, m: u32, s: u32, ns: u32) -> NaiveTime {
        NaiveTime::from_hms_nano_opt(h, m, s, ns).unwrap()
    }

    #[test]
    fn timecode_frames() {
        assert_eq!(timecode_fields(&at(13, 32, 1, 0), 30), (13, 32, 1, 0));
        assert_eq!(timecode_fields(&at(13, 32, 1, 433_333_334), 30), (13, 32, 1, 13));
        assert_eq!(timecode_fields(&at(23, 59, 59, 999_999_999), 30), (23, 59, 59, 29));
        assert_eq!(timecode_fields(&at(23, 59, 59, 999_999_999), 60), (23, 59, 59, 59));
        assert_eq!(timecode_fields(&at(0, 0, 0, 16_666_667), 60), (0, 0, 0, 1));
    }

    #[test]
    fn bwf_time_reference() {
        assert_eq!(samples_since_midnight(&at(0, 0, 0, 0), 48_000), 0);
        assert_eq!(
            samples_since_midnight(&at(13, 32, 1, 500_000_000), 48_000),
            (13 * 3600 + 32 * 60 + 1) * 48_000 + 24_000
        );
        assert_eq!(samples_since_midnight(&at(23, 59, 59, 999_999_999), 48_000), 86_400 * 48_000 - 1);
    }

    #[test]
    fn capture_time_uses_running_time() {
        let base = gst::ClockTime::from_seconds(1_000);
        let pts = gst::ClockTime::from_mseconds(1_500);
        assert_eq!(capture_unix_ns(base, None, pts), 1_001_500_000_000);
        let mut segment = gst::FormattedSegment::<gst::ClockTime>::new();
        segment.set_start(gst::ClockTime::from_mseconds(500));
        assert_eq!(capture_unix_ns(base, Some(&segment), pts), 1_001_000_000_000);
    }
}
