//! Telemetry: GPS (modem GNSS), modem state, UPS and system state. Each source is a supervised task that
//! samples at its own period and writes one CSV line per sample (SPEC §5b); an unavailable value leaves an
//! empty field, a line is never skipped.

mod chrony;
mod csv;
pub mod gps;
mod mm;
pub mod modem;
mod nmea;
pub mod system;
pub mod ups;

use std::collections::BTreeMap;
use std::fmt::Write as _;
use std::path::PathBuf;
use std::sync::Mutex;

use tokio::sync::watch;

pub use csv::CsvRecorder;

/// Where a telemetry source writes: CSV files (following the recording gate) and the live snapshot.
#[derive(Clone)]
pub struct Output {
    pub dir: PathBuf,
    pub gate: watch::Receiver<bool>,
    pub snapshot: std::sync::Arc<Snapshot>,
}

/// Latest sample of every telemetry source as JSON, published in the LiveKit room metadata. Updated on
/// every sample, whether local recording runs or not.
pub struct Snapshot {
    sections: Mutex<BTreeMap<&'static str, serde_json::Value>>,
    version: watch::Sender<u64>,
}

impl Default for Snapshot {
    fn default() -> Self {
        Self { sections: Mutex::new(BTreeMap::new()), version: watch::channel(0).0 }
    }
}

impl Snapshot {
    pub fn update(&self, source: &'static str, section: serde_json::Value) {
        self.sections.lock().unwrap_or_else(std::sync::PoisonError::into_inner).insert(source, section);
        self.version.send_modify(|v| *v = v.wrapping_add(1));
    }

    pub fn sections(&self) -> BTreeMap<&'static str, serde_json::Value> {
        self.sections.lock().unwrap_or_else(std::sync::PoisonError::into_inner).clone()
    }

    /// Notified on every update.
    pub fn subscribe(&self) -> watch::Receiver<u64> {
        self.version.subscribe()
    }
}

/// One CSV field. `None` = value unavailable (empty field).
#[derive(Debug, Clone, PartialEq)]
pub enum Value {
    /// Number with a fixed number of decimals.
    Float(Option<f64>, usize),
    Int(Option<i64>),
    Text(Option<String>),
    Bool(Option<bool>),
}

impl Value {
    pub fn float(v: Option<f64>, decimals: usize) -> Self {
        Self::Float(v.filter(|v| v.is_finite()), decimals)
    }

    pub fn int(v: Option<impl Into<i64>>) -> Self {
        Self::Int(v.map(Into::into))
    }

    pub fn text(v: Option<impl Into<String>>) -> Self {
        Self::Text(v.map(Into::into))
    }

    /// JSON representation: numbers rounded like in the CSV, `null` when unavailable.
    fn to_json(&self) -> serde_json::Value {
        use serde_json::Value as J;
        match self {
            Self::Float(Some(v), decimals) => {
                let factor = 10f64.powi(i32::try_from(*decimals).unwrap_or(6));
                serde_json::Number::from_f64((v * factor).round() / factor).map_or(J::Null, J::Number)
            }
            Self::Int(Some(v)) => J::from(*v),
            Self::Bool(Some(v)) => J::Bool(*v),
            Self::Text(Some(t)) => J::String(t.clone()),
            Self::Float(None, _) | Self::Int(None) | Self::Bool(None) | Self::Text(None) => J::Null,
        }
    }

    /// CSV representation (RFC 4180 quoting for text containing a separator, a quote or a newline).
    fn write_csv(&self, out: &mut String) {
        match self {
            Self::Float(Some(v), decimals) => {
                let _ = write!(out, "{v:.decimals$}");
            }
            Self::Int(Some(v)) => {
                let _ = write!(out, "{v}");
            }
            Self::Bool(Some(v)) => out.push_str(if *v { "true" } else { "false" }),
            Self::Text(Some(t)) if t.contains([',', '"', '\n', '\r']) => {
                out.push('"');
                out.push_str(&t.replace('"', "\"\""));
                out.push('"');
            }
            Self::Text(Some(t)) => out.push_str(t),
            Self::Float(None, _) | Self::Int(None) | Self::Bool(None) | Self::Text(None) => {}
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    fn csv(v: Value) -> String {
        let mut s = String::new();
        v.write_csv(&mut s);
        s
    }

    #[test]
    fn csv_values() {
        assert_eq!(csv(Value::float(Some(12.4867), 3)), "12.487");
        assert_eq!(csv(Value::float(Some(f64::NAN), 3)), "");
        assert_eq!(csv(Value::float(None, 2)), "");
        assert_eq!(csv(Value::int(Some(-97i32))), "-97");
        assert_eq!(csv(Value::Bool(Some(true))), "true");
        assert_eq!(csv(Value::text(Some("Orange F"))), "Orange F");
        assert_eq!(csv(Value::text(Some("a,\"b\""))), "\"a,\"\"b\"\"\"");
        assert_eq!(csv(Value::text(None::<String>)), "");
    }

    #[test]
    fn json_values() {
        assert_eq!(Value::float(Some(12.4867), 3).to_json(), serde_json::json!(12.487));
        assert_eq!(Value::float(None, 3).to_json(), serde_json::Value::Null);
        assert_eq!(Value::int(Some(8u32)).to_json(), serde_json::json!(8));
        assert_eq!(Value::text(Some("3d")).to_json(), serde_json::json!("3d"));
    }
}
