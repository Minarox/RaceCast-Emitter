# RaceCast — Protocol towards the front end

This project defines the formats (SPEC §6, decision of 2026-09-28). This document is the contract the front
end relies on. Any breaking change increments the metadata version `v`.

## Room and participant

| Item | Value |
|---|---|
| Room | `LIVEKIT_ROOM` (default `racecast`), created by the car with 24 h empty/departure timeouts |
| Car participant | identity `LIVEKIT_IDENTITY` (default `racecast-car`), publishes only, never subscribes |
| Cars per room | one |

The room outlives the car's connection: when the car is offline (tunnel, no 5G), its participant and tracks
disappear but the room metadata keeps the last known state.

**Viewer tokens must carry the same room configuration**: `roomConfig` with `empty_timeout` and
`departure_timeout` set to 86400 s. After 24 h without the car, the room is gone, and the next viewer's
join creates it again. The server applies the token's configuration only to a room created by that join.
The car's `CreateRoom` does not change the timeouts of a room that already exists. Without this
configuration, such a room would get the server defaults and close shortly after the car leaves, taking
the last known state with it.

## Tracks

| Kind | Source | Codec | Name |
|---|---|---|---|
| Video, one per camera | `camera` | **AV1** (single layer, no simulcast) | device name, e.g. `cam-front` |
| Audio, one per microphone | `microphone` | Opus | device name, e.g. `mic-driver` |

- Device names are unique across cameras and microphones (`devices.yml`, or automatic `cam-<model>` /
  `mic-<model>`).
- Default video: 960×540 @ 30 fps, 1.2 Mbps (the aspect ratio of the camera is kept; a camera rotated by
  90° gives a portrait track). The main camera may have its own settings, and gets most of the car's
  uplink when it is short (the other cameras lower their bitrate, none is stopped).
- Viewers must decode AV1 (Chrome, Firefox, Edge; Safari only on hardware with an AV1 decoder).
- Tracks come and go with the devices (hot-plug) and with the connection.

## Participant attributes

| Attribute | Value |
|---|---|
| `main_camera` | Track name of the main camera. **Absent** when none is designated: LiveKit deletes an attribute set to an empty string |

## Room metadata

A single JSON document, replaced at most once per second while the car is online (one write per update,
whatever the number of viewers). Listen to `RoomMetadataChanged`; the current value is also in the room
info when joining.

```json
{
  "v": 1,
  "ts": "2026-09-28T12:34:25.849Z",
  "car": {
    "recording": true,
    "main_camera": "cam-front",
    "cameras": [
      { "name": "cam-front", "main": true, "streaming": true },
      { "name": "cam-rear", "main": false, "streaming": true }
    ],
    "microphones": [{ "name": "mic-driver", "streaming": true }]
  },
  "gps": { "ts": "…", "fix": "3d", "lat": 48.1173, "lon": 11.5166667, "…": "…" },
  "modem": { "ts": "…", "state": "connected", "…": "…" },
  "ups": { "ts": "…", "load_voltage_v": 12.492, "…": "…" },
  "system": { "ts": "…", "cpu_temp_c": 52.2, "…": "…" }
}
```

- `ts` (root): when the document was built (UTC). Each section has its own `ts`: time of that sample. Use
  them to show "last updated X ago", in particular when the car participant is absent.
- `car.recording`: local recording running (requested and enough disk space).
- `car.cameras` / `car.microphones`: devices currently plugged in and handled (with streaming enabled);
  `streaming` = track published in the room.
- The document is about 1.2 KB with 2 cameras and 2 microphones.
- A telemetry section appears after its first sample. Every field is present in a section; `null` means
  "value unavailable" (no GPS fix, modem absent, sensor unreadable…).

### `gps` (every second)

| Field | Type | Unit / values |
|---|---|---|
| `fix` | string | `none`, `2d`, `3d` (position fields are `null` without a fix) |
| `lat`, `lon` | number | decimal degrees (WGS84), 7 decimals |
| `alt_m` | number | metres |
| `speed_kmh` | number | km/h |
| `course_deg` | number | degrees from true north |
| `satellites` | integer | satellites used |
| `hdop` | number | horizontal dilution of precision |
| `gps_time` | string | UTC time given by the GNSS (ISO 8601) |

### `modem` (every 5 s)

| Field | Type | Unit / values |
|---|---|---|
| `state` | string | `connected`, `registered`, `searching`, `enabled`, `failed`, `absent`, … |
| `access_tech` | string | e.g. `lte+5gnr`, `lte` |
| `operator` | string | e.g. `Orange F` |
| `signal_quality` | integer | % |
| `lte_rssi_dbm`, `lte_rsrp_dbm` | number | dBm (LTE anchor) |
| `lte_rsrq_db`, `lte_sinr_db` | number | dB |
| `nr_rsrp_dbm` | number | dBm (5G NR carrier) |
| `nr_rsrq_db`, `nr_sinr_db` | number | dB |
| `cell_id`, `tac` | string | hexadecimal, as reported by the modem |
| `ip_connected` | boolean | data bearer up |

### `ups` (every 2 s)

| Field | Type | Unit |
|---|---|---|
| `load_voltage_v` | number | V |
| `current_a` | number | A (signed) |
| `power_w` | number | W |
| `percent` | number | % (3S pack, 9 V = 0 %, 12.6 V = 100 %) |

### `system` (every 5 s)

| Field | Type | Unit / values |
|---|---|---|
| `cpu_temp_c`, `gpu_temp_c`, `tj_temp_c` | number | °C (`tj` = hottest junction) |
| `cpu_load_pct`, `gpu_load_pct` | number | % (whole machine) |
| `ram_used_mb` | integer | MB |
| `nvenc_mhz` | integer | video encoder clock (load proxy) |
| `disk_free_gb` | number | GB free for the recordings |
| `recording` | boolean | local recording running |
| `cameras`, `mics` | integer | devices handled |
| `livekit_connected` | boolean | car connected to the room |
| `power_mode` | string | Jetson power mode, e.g. `15W` |

## Later

RPC methods of the admin page (device configuration, recording start/stop) will be documented here.
