//! CSV files of a telemetry source: `RECORDINGS_DIR/<YYYY-MM-DD>/<source>_<YYYY-MM-DDTHH-MM-SS>.csv`,
//! header on the first line, one line per sample, flushed immediately. Follows the recording gate like the
//! audio/video files: a new file when the gate opens, closed when it closes. Every sample also updates the
//! live snapshot (LiveKit room metadata), gate or not.

use std::fs::File;
use std::io::{self, BufWriter, Write};
use std::path::PathBuf;
use std::time::{Duration, Instant};

use tokio::sync::watch;

use super::{Output, Snapshot, Value};
use crate::recording::new_file_path;

const RETRY: Duration = Duration::from_secs(5);

pub struct CsvRecorder {
    source: &'static str,
    /// Columns after `timestamp`.
    columns: &'static [&'static str],
    dir: PathBuf,
    gate: watch::Receiver<bool>,
    snapshot: std::sync::Arc<Snapshot>,
    file: Option<(BufWriter<File>, PathBuf)>,
    retry_at: Option<Instant>,
}

impl CsvRecorder {
    pub fn new(source: &'static str, columns: &'static [&'static str], out: Output) -> Self {
        Self { source, columns, dir: out.dir, gate: out.gate, snapshot: out.snapshot, file: None, retry_at: None }
    }

    /// Writes one sample (`values` in the order of the columns), stamped with the current UTC time. Never
    /// fails: file errors are logged and retried after a delay.
    pub fn record(&mut self, values: &[Value]) {
        debug_assert_eq!(values.len(), self.columns.len(), "{}: column count mismatch", self.source);
        let timestamp = chrono::Utc::now().format("%Y-%m-%dT%H:%M:%S%.3fZ").to_string();
        let mut section = serde_json::Map::new();
        section.insert("ts".into(), timestamp.clone().into());
        for (column, v) in self.columns.iter().zip(values) {
            section.insert((*column).into(), v.to_json());
        }
        self.snapshot.update(self.source, serde_json::Value::Object(section));

        if !*self.gate.borrow() {
            self.close();
            return;
        }
        if self.file.is_none() {
            if self.retry_at.is_some_and(|t| Instant::now() < t) {
                return;
            }
            self.retry_at = None;
            if let Err(e) = self.open() {
                tracing::error!(error = %e, retry_s = RETRY.as_secs(), "cannot start the telemetry file");
                self.retry_at = Some(Instant::now() + RETRY);
                return;
            }
        }
        let mut line = timestamp;
        for v in values {
            line.push(',');
            v.write_csv(&mut line);
        }
        line.push('\n');
        if let Some((out, path)) = &mut self.file
            && let Err(e) = out.write_all(line.as_bytes()).and_then(|()| out.flush())
        {
            tracing::error!(path = %path.display(), error = %e, retry_s = RETRY.as_secs(), "telemetry write failed");
            self.file = None;
            self.retry_at = Some(Instant::now() + RETRY);
        }
    }

    fn open(&mut self) -> io::Result<()> {
        let path = new_file_path(&self.dir, self.source, &chrono::Local::now(), "csv")?;
        let mut out = BufWriter::new(File::create(&path)?);
        writeln!(out, "timestamp,{}", self.columns.join(","))?;
        out.flush()?;
        tracing::info!(path = %path.display(), "telemetry recording started");
        self.file = Some((out, path));
        Ok(())
    }

    /// Closes the current file (flushed to disk).
    pub fn close(&mut self) {
        if let Some((out, path)) = self.file.take() {
            match out.into_inner().map_err(|e| e.into_error()).and_then(|f| f.sync_all()) {
                Ok(()) => tracing::info!(path = %path.display(), "telemetry recording finalized"),
                Err(e) => tracing::warn!(path = %path.display(), error = %e, "telemetry file closed with an error"),
            }
        }
    }
}

impl Drop for CsvRecorder {
    fn drop(&mut self) {
        self.close();
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn writes_header_and_lines_while_the_gate_is_open() {
        let dir = std::env::temp_dir().join(format!("racecast-csv-{}", std::process::id()));
        let (gate, rx) = watch::channel(false);
        let snapshot = std::sync::Arc::new(Snapshot::default());
        let out = Output { dir: dir.clone(), gate: rx, snapshot: snapshot.clone() };
        let mut rec = CsvRecorder::new("ups", &["voltage_v", "note"], out);
        rec.record(&[Value::float(Some(12.5), 2), Value::text(Some("x"))]);
        assert!(!dir.exists(), "no file while the gate is closed");
        let section = &snapshot.sections()["ups"];
        assert_eq!(section["voltage_v"], serde_json::json!(12.5));
        assert!(section["ts"].as_str().is_some_and(|t| t.ends_with('Z')));

        gate.send_replace(true);
        rec.record(&[Value::float(Some(12.5), 2), Value::text(Some("a,b"))]);
        rec.record(&[Value::float(None, 2), Value::text(None::<String>)]);
        drop(rec);

        let day = std::fs::read_dir(&dir).unwrap().next().unwrap().unwrap().path();
        let file = std::fs::read_dir(&day).unwrap().next().unwrap().unwrap().path();
        let text = std::fs::read_to_string(&file).unwrap();
        std::fs::remove_dir_all(&dir).unwrap();
        let lines: Vec<&str> = text.lines().collect();
        assert_eq!(lines[0], "timestamp,voltage_v,note");
        assert!(lines[1].ends_with("Z,12.50,\"a,b\""), "{}", lines[1]);
        assert!(lines[2].ends_with("Z,,"), "{}", lines[2]);
        assert!(file.file_name().unwrap().to_string_lossy().starts_with("ups_"));
    }
}
