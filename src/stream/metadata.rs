//! Room metadata publisher (SPEC §6): the full state — telemetry sections and car state — as one JSON
//! document, written through the server API at most once per second. At most one request in flight; while
//! a request is slow or failing, newer states overwrite older ones (never queued). The schema is documented
//! in `docs/PROTOCOL.md`.

use std::sync::Arc;
use std::time::Duration;

use livekit_api::services::room::RoomClient;
use serde_json::{Map, Value, json};
use tokio::sync::watch;
use tokio_util::sync::CancellationToken;

use super::StreamHub;
use super::room::{api_url, ensure_room};
use crate::capture::DeviceKind;
use crate::config::LiveKitSettings;
use crate::telemetry::Snapshot;

pub const VERSION: u64 = 1;
const MIN_INTERVAL: Duration = Duration::from_secs(1);
/// Retry period while the server is unreachable.
const RETRY: Duration = Duration::from_secs(10);

pub struct Sources {
    pub snapshot: Arc<Snapshot>,
    pub hub: Arc<StreamHub>,
    pub recording: watch::Receiver<bool>,
}

/// Builds the metadata document.
pub fn document(src: &Sources) -> Value {
    let mut doc = Map::new();
    doc.insert("v".into(), json!(VERSION));
    doc.insert("ts".into(), json!(chrono::Utc::now().format("%Y-%m-%dT%H:%M:%S%.3fZ").to_string()));
    let states = src.hub.states();
    let list = |kind: DeviceKind| -> Vec<Value> {
        states
            .iter()
            .filter(|s| s.kind == kind)
            .map(|s| match kind {
                DeviceKind::Camera => json!({ "name": s.name, "main": s.main, "streaming": s.published }),
                DeviceKind::Microphone => json!({ "name": s.name, "streaming": s.published }),
            })
            .collect()
    };
    let main = states.iter().find(|s| s.kind == DeviceKind::Camera && s.main).map(|s| s.name.clone());
    doc.insert(
        "car".into(),
        json!({
            "recording": *src.recording.borrow(),
            "main_camera": main,
            "cameras": list(DeviceKind::Camera),
            "microphones": list(DeviceKind::Microphone),
        }),
    );
    for (source, section) in src.snapshot.sections() {
        doc.insert(source.into(), section);
    }
    Value::Object(doc)
}

/// Publishes the metadata until `token` is cancelled.
pub async fn run(src: Sources, lk: LiveKitSettings, token: CancellationToken) -> Result<(), String> {
    let client = RoomClient::with_api_key(&api_url(&lk.url), &lk.api_key, &lk.api_secret);
    let mut telemetry = src.snapshot.subscribe();
    let mut recording = src.recording.clone();
    let mut registrations = src.hub.changed.subscribe();
    let mut failing = false;
    loop {
        let started = tokio::time::Instant::now();
        let doc = document(&src).to_string();
        let result = match client.update_room_metadata(&lk.room, &doc).await.map_err(|e| e.to_string()) {
            // The room does not exist yet (car not connected since the server started): create it, retry.
            Err(e) if e.contains("not_found") || e.contains("does not exist") => match ensure_room(&lk).await {
                Ok(()) => client.update_room_metadata(&lk.room, &doc).await.map(|_| ()).map_err(|e| e.to_string()),
                Err(e) => Err(e),
            },
            other => other.map(|_| ()),
        };
        match result {
            Ok(()) => {
                if std::mem::take(&mut failing) {
                    tracing::info!(bytes = doc.len(), "room metadata published again");
                }
            }
            Err(e) => {
                if !std::mem::replace(&mut failing, true) {
                    tracing::warn!(error = %e, "cannot publish the room metadata");
                }
            }
        }
        // Next update: after a change, never more often than once per second.
        let wait = if failing { RETRY } else { MIN_INTERVAL };
        tokio::select! {
            () = token.cancelled() => return Ok(()),
            () = tokio::time::sleep_until(started + wait) => {}
        }
        if !failing {
            tokio::select! {
                () = token.cancelled() => return Ok(()),
                r = telemetry.changed() => r.map_err(|_| "telemetry snapshot closed")?,
                r = recording.changed() => r.map_err(|_| "recording gate closed")?,
                r = registrations.changed() => r.map_err(|_| "stream hub closed")?,
            }
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn document_layout() {
        let snapshot = Arc::new(Snapshot::default());
        snapshot.update("ups", json!({ "ts": "2026-09-28T12:00:00.000Z", "percent": 97.0 }));
        let src = Sources { snapshot, hub: StreamHub::new(), recording: watch::channel(true).1 };
        let doc = document(&src);
        assert_eq!(doc["v"], json!(1));
        assert_eq!(doc["car"]["recording"], json!(true));
        assert_eq!(doc["car"]["main_camera"], Value::Null);
        assert_eq!(doc["car"]["cameras"], json!([]));
        assert_eq!(doc["ups"]["percent"], json!(97.0));
        assert!(doc.to_string().len() < 2048);
    }
}
