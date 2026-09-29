//! NMEA 0183 parsing (RMC, GGA, GSA from any talker: GP, GN, GL, GA, GB…) and the resulting GPS state.

use std::time::{Duration, Instant};

use chrono::{DateTime, NaiveDate, NaiveTime, Utc};

use super::Value;

/// A fix older than this is reported as "no fix".
const STALE: Duration = Duration::from_secs(3);
const KNOT_KMH: f64 = 1.852;

#[derive(Debug, Clone, PartialEq)]
pub enum Sentence {
    Rmc {
        valid: bool,
        time: Option<NaiveTime>,
        date: Option<NaiveDate>,
        lat: Option<f64>,
        lon: Option<f64>,
        speed_knots: Option<f64>,
        course: Option<f64>,
    },
    Gga {
        quality: u8,
        satellites: Option<u32>,
        hdop: Option<f64>,
        alt_m: Option<f64>,
    },
    Gsa {
        mode: u8,
        hdop: Option<f64>,
    },
}

impl Sentence {
    /// GNSS time of a valid fix (RMC with status `A`), used as a time source for chrony.
    pub fn fix_time(&self) -> Option<DateTime<Utc>> {
        match self {
            Sentence::Rmc { valid: true, time: Some(t), date: Some(d), .. } => Some(d.and_time(*t).and_utc()),
            _ => None,
        }
    }
}

/// Parses one line. `None` for an invalid checksum, a malformed line or an unused sentence type.
pub fn parse(line: &str) -> Option<Sentence> {
    let line = line.trim();
    let body = line.strip_prefix('$')?;
    let (data, checksum) = body.rsplit_once('*')?;
    let expected = u8::from_str_radix(checksum.get(..2)?, 16).ok()?;
    if data.bytes().fold(0u8, |acc, b| acc ^ b) != expected {
        return None;
    }
    let fields: Vec<&str> = data.split(',').collect();
    let kind = fields.first()?.get(2..)?;
    let f = |i: usize| fields.get(i).copied().filter(|s| !s.is_empty());
    let num = |i: usize| f(i).and_then(|s| s.parse::<f64>().ok());
    match kind {
        "RMC" => Some(Sentence::Rmc {
            valid: f(2) == Some("A"),
            time: f(1).and_then(parse_time),
            date: f(9).and_then(|d| NaiveDate::parse_from_str(d, "%d%m%y").ok()),
            lat: coord(f(3), f(4)),
            lon: coord(f(5), f(6)),
            speed_knots: num(7),
            course: num(8),
        }),
        "GGA" => Some(Sentence::Gga {
            quality: f(6).and_then(|q| q.parse().ok()).unwrap_or(0),
            satellites: f(7).and_then(|s| s.parse().ok()),
            hdop: num(8),
            alt_m: num(9),
        }),
        "GSA" => Some(Sentence::Gsa { mode: f(2).and_then(|m| m.parse().ok()).unwrap_or(1), hdop: num(16) }),
        _ => None,
    }
}

fn parse_time(s: &str) -> Option<NaiveTime> {
    NaiveTime::parse_from_str(s, "%H%M%S%.f").or_else(|_| NaiveTime::parse_from_str(s, "%H%M%S")).ok()
}

/// `ddmm.mmmm` + hemisphere → signed decimal degrees.
fn coord(value: Option<&str>, hemisphere: Option<&str>) -> Option<f64> {
    let v: f64 = value?.parse().ok()?;
    let degrees = (v / 100.0).trunc();
    let decimal = degrees + (v - degrees * 100.0) / 60.0;
    match hemisphere? {
        "N" | "E" => Some(decimal),
        "S" | "W" => Some(-decimal),
        _ => None,
    }
}

/// Latest GPS state built from the sentences received.
#[derive(Debug, Default)]
pub struct GpsState {
    valid_at: Option<Instant>,
    mode: Option<u8>,
    lat: Option<f64>,
    lon: Option<f64>,
    alt_m: Option<f64>,
    speed_kmh: Option<f64>,
    course: Option<f64>,
    satellites: Option<u32>,
    hdop: Option<f64>,
    gps_time: Option<DateTime<Utc>>,
}

pub const COLUMNS: &[&str] =
    &["fix", "lat", "lon", "alt_m", "speed_kmh", "course_deg", "satellites", "hdop", "gps_time"];

impl GpsState {
    pub fn update(&mut self, sentence: Sentence, now: Instant) {
        match sentence {
            Sentence::Rmc { valid, time, date, lat, lon, speed_knots, course } => {
                if valid {
                    self.valid_at = Some(now);
                    (self.lat, self.lon, self.course) = (lat, lon, course);
                    self.speed_kmh = speed_knots.map(|k| k * KNOT_KMH);
                }
                if let (Some(d), Some(t)) = (date, time) {
                    self.gps_time = Some(d.and_time(t).and_utc());
                }
            }
            Sentence::Gga { quality, satellites, hdop, alt_m } => {
                self.satellites = satellites;
                self.hdop = hdop.or(self.hdop);
                self.alt_m = if quality > 0 { alt_m } else { None };
            }
            Sentence::Gsa { mode, hdop } => {
                self.mode = Some(mode);
                self.hdop = hdop.or(self.hdop);
            }
        }
    }

    /// `none`, `2d` or `3d`.
    pub fn fix(&self, now: Instant) -> &'static str {
        if !self.valid_at.is_some_and(|t| now.duration_since(t) <= STALE) {
            return "none";
        }
        match self.mode {
            Some(2) => "2d",
            Some(3) => "3d",
            _ if self.alt_m.is_some() => "3d",
            _ => "2d",
        }
    }

    /// One CSV sample (see [`COLUMNS`]); position fields are empty without a fix.
    pub fn row(&self, now: Instant) -> Vec<Value> {
        let fix = self.fix(now);
        let has_fix = fix != "none";
        let pos = |v: Option<f64>| v.filter(|_| has_fix);
        vec![
            Value::text(Some(fix)),
            Value::float(pos(self.lat), 7),
            Value::float(pos(self.lon), 7),
            Value::float(pos(self.alt_m), 1),
            Value::float(pos(self.speed_kmh), 1),
            Value::float(pos(self.course), 1),
            Value::int(self.satellites),
            Value::float(pos(self.hdop), 1),
            Value::text(self.gps_time.map(|t| t.format("%Y-%m-%dT%H:%M:%S%.3fZ").to_string())),
        ]
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn checksum_and_unused_sentences() {
        assert!(parse("$GPGGA,,,,,,0,,,,,,,,*66").is_some());
        assert!(parse("$GPGGA,,,,,,0,,,,,,,,*67").is_none());
        assert!(parse("$GPVTG,,T,,M,,N,,K,N*2C").is_none());
        assert!(parse("garbage").is_none());
    }

    #[test]
    fn no_fix_sentences_from_the_modem() {
        let now = Instant::now();
        let mut s = GpsState::default();
        for l in ["$GPGGA,,,,,,0,,,,,,,,*66", "$GPRMC,,V,,,,,,,,,,N,V*29", "$GPGSA,A,1,,,,,,,,,,,,,,,,*32"] {
            s.update(parse(l).unwrap(), now);
        }
        assert_eq!(s.fix(now), "none");
        let row = s.row(now);
        assert_eq!(row[0], Value::text(Some("none")));
        assert!(row[1..].iter().all(|v| matches!(v, Value::Float(None, _) | Value::Int(None) | Value::Text(None))));
    }

    #[test]
    fn fix_from_real_sentences() {
        let now = Instant::now();
        let mut s = GpsState::default();
        let lines = [
            "$GPRMC,123519,A,4807.038,N,01131.000,E,022.4,084.4,230394,003.1,W*6A",
            "$GPGGA,123519,4807.038,N,01131.000,E,1,08,0.9,545.4,M,46.9,M,,*47",
            "$GPGSA,A,3,04,05,,09,12,,,24,,,,,2.5,1.3,2.1*39",
        ];
        for l in lines {
            s.update(parse(l).unwrap_or_else(|| panic!("{l}")), now);
        }
        assert_eq!(s.fix(now), "3d");
        let csv: Vec<String> = s
            .row(now)
            .iter()
            .map(|v| {
                let mut out = String::new();
                v.write_csv(&mut out);
                out
            })
            .collect();
        assert_eq!(
            csv,
            ["3d", "48.1173000", "11.5166667", "545.4", "41.5", "84.4", "8", "1.3", "1994-03-23T12:35:19.000Z"]
        );
        // Stale fix → reported as none.
        assert_eq!(s.fix(now + Duration::from_secs(4)), "none");
    }

    #[test]
    fn fix_time_only_from_valid_rmc() {
        let valid = parse("$GPRMC,123519,A,4807.038,N,01131.000,E,022.4,084.4,230394,003.1,W*6A").unwrap();
        assert_eq!(valid.fix_time().map(|t| t.to_rfc3339()), Some("1994-03-23T12:35:19+00:00".into()));
        assert_eq!(parse("$GPRMC,,V,,,,,,,,,,N,V*29").unwrap().fix_time(), None);
        assert_eq!(parse("$GPGGA,,,,,,0,,,,,,,,*66").unwrap().fix_time(), None);
    }

    #[test]
    fn southern_and_western_hemispheres() {
        assert_eq!(coord(Some("3352.128"), Some("S")), Some(-(33.0 + 52.128 / 60.0)));
        assert_eq!(coord(Some("15112.500"), Some("W")), Some(-(151.0 + 12.5 / 60.0)));
        assert_eq!(coord(Some("4807.038"), Some("X")), None);
    }
}
