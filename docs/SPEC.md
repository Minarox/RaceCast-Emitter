# RaceCast-Emitter — Specification

Detailed requirements and design decisions. The non-negotiable rules and working conventions are
summarized in `CLAUDE.md`; this document is authoritative for the details.

## 1. Goal

Rust program embedded in a race car that:

1. **Captures** camera and microphone streams and telemetry (GPS, modem, UPS, system state).
2. **Records** these streams locally (source of truth).
3. **Publishes** these streams simultaneously in real time to a **LiveKit** room (WebRTC).

Core constraint: **fully autonomous and fault tolerant** (network loss, unplugged device, reboot, power
cut). A partial failure is absorbed without interrupting the other streams or killing the process.

## 2. Hardware

| Item | Details |
|---|---|
| Compute | NVIDIA Jetson Orin NX (Engineering Reference Developer Kit) |
| Power | [Waveshare UPS Power Module C](https://www.waveshare.com/UPS-Power-Module-C.htm): INA219 on I2C bus 7, address `0x41` (detected). Reference Python script: `/home/jetson/Projects/UPS_Power_Module_C/ina219.py` |
| Connectivity | 5G HAT, Quectel RM520N-GL modem (managed by ModemManager, QMI port `cdc-wdm0`) — intermittent by design |
| Cameras | USB UVC/V4L2, 3 planned (the 3rd one, not here yet, does 1080p60) |
| Microphones | USB UAC (built into cameras or separate USB microphones) |
| Telemetry | **GPS**: GNSS built into the RM520N-GL modem (NMEA on `ttyUSB1`, no other module planned); **modem** (network state); **UPS**. CAN bus in **phase 2** (connection not defined — open architecture, not a prerequisite) |
| Storage | 256 GB NVMe SSD **shared with the system** (~130 GB free on 2026-09-25), soon replaced by a 500 GB one |

### Devices observed on the machine (2026-09-25)

| by-id | Useful formats | Built-in audio |
|---|---|---|
| `usb-Generic_USB_Camera_200901010001` | MJPEG 1080p30, 720p30 (YUYV unusable: 1080p3) | yes (`card 2`) |
| `usb-HD_USB_Camera_HD_USB_Camera` | MJPEG 1080p30, **720p60** (YUYV: 1080p6) | no |
| `usb-C-Media_Electronics_Inc._USB_Audio_Device` | — | USB microphone (`card 3`) |

None of the current cameras does 1080p60. The serial number `200901010001` looks like a generic factory
value, and `HD_USB_Camera` has none: two identical units would be indistinguishable by by-id (hence the
identification by USB port, §8).

**USB topology (2026-09-28)**: both cameras (ports `2.1` and `2.4`), the C-Media microphone (`2.2`) **and
the 5G modem** (`2.3`) all sit on **the same USB 2.0 hub (480 Mbit/s)**; the USB 3 bus (10 Gbit/s) is
empty. A 1080p MJPEG UVC camera reserves a large share of the USB 2 isochronous bandwidth: a 3rd camera
(1080p60) may be refused when opened (`ENOSPC` / "No space left on device") or disturb the modem. To check
with the 3rd camera: distribution across physical ports, USB 3 bus if the camera supports it.

## 3. Hardware acceleration & energy efficiency

The Jetson may run on battery for a long time: any CPU processing where a hardware block exists costs
battery life. Explicit criterion for every technical choice.

- **Capture**: MJPEG from the camera (YUYV is unusable at these resolutions anyway).
- **MJPEG decoding**: hardware — `nvv4l2decoder mjpeg=1` or `nvjpegdec` (both available; choose by
  measuring CPU/latency).
- **Conversion / scaling / rotation**: `nvvidconv` (VIC), never `videoconvert`/`videoscale`/`videoflip`.
- **Video encoding**: NVENC only — H.264 (`nvv4l2h264enc`) for recording, AV1 for streaming (Jetson
  encoder of the LiveKit SDK or `nvv4l2av1enc`). Never `x264enc` or any other software encoder.
- **NVENC load**: 3 cameras × 2 encodes = 3 H.264 sessions (one of them 1080p60) + 3 simultaneous AV1
  540p30 sessions, plus 3 MJPEG decodes — to be validated under real load.
- **System power**: `nvpmodel`/`jetson_clocks` to adapt the performance mode (especially on battery), to
  be refined with real measurements.

## 4. Architecture

Every source (camera N, microphone N, GPS, modem, UPS, later CAN) = **isolated, supervised task, restartable
independently** (retry with backoff). Capture runs permanently and is split (`tee`) into two branches with
independent life cycles and error handling:

1. **Local recording** — top priority, critical, native quality. **Never stops** because of the LiveKit
   branch (5G loss, disconnection, WebRTC error).
2. **LiveKit streaming** — best-effort, reduced resolution/bitrate, reconnection with backoff, no catch-up
   of missed data.

A failure on the streaming side must never propagate to recording (e.g. `queue leaky=downstream` on the
stream branch, isolated bus/errors).

Video pipeline outline:

```
v4l2src (MJPEG) → HW decoder → nvvidconv (rotation) → tee
  ├─ queue → nvv4l2h264enc (native, 12 Mbps VBR) → h264parse → [timecode probe] → qtmux (fragmented) → filesink
  └─ leaky queue → nvvidconv (downscale 540p) → NVENC AV1 encoding (1.2 Mbps) → LiveKit publication
```

Telemetry: same principle — complete local CSV that never stops + best-effort LiveKit publication through
the room metadata (§5b, §6).

## 5. Local recording

### Video — decisions

- **Codec**: H.264 via `nvv4l2h264enc`, **VBR** rate control, **12 Mbps** target per camera (configurable
  in `devices.yml`, to be refined by real tests). High profile.
- **Container**: **fragmented QuickTime `.mov`** via `qtmux fragment-duration=1000
  fragment-mode=first-moov-then-finalise`. **1 s** fragments with aligned keyframe/IDR
  (`iframeinterval` = `idrinterval` = fps, `insert-sps-pps=true`) → at most ≈ 1 s lost on a hard power
  cut. On a clean stop, qtmux finalizes the file into a regular `.mov` (complete moov at the end of the
  file, without copying the data).
- **Timecode (mandatory)**: QuickTime `tmcd` track (rate = fps selected for the camera, e.g. 30 or 60 fps,
  24 h), time of day from the system clock (§7), set by a **pad probe** on the `h264parse` output that
  attaches a `VideoTimeCodeMeta` to every buffer, read by qtmux. **The timecode is derived from the
  capture timestamp** of the buffer (`base_time + PTS`, pipeline clocked by `GstSystemClock` with
  `clock-type=realtime`), never from the arrival time in the probe (variable encoding latency).
- **Constant frame rate**: `videorate skip-to-first=true` in NVMM memory (zero copy: duplicates/drops
  references) between `nvv4l2decoder` and `nvvidconv`. Without it, USB cameras deliver irregular
  timestamps (28 fps on average, durations 96–276/3000) that Resolve handles poorly. `skip-to-first=true`
  is **essential**: by default videorate moves the first frame to the start of the segment and makes the
  start timecode wrong.
- **Rejected** (tests on 2026-09-25, see §12): `isofmp4mux` and `mp4mux` (no `tmcd` track, including on the
  main branch of gst-plugins-rs); `qtmux fragment-mode=dash-or-mss` (GStreamer 1.24.2 bug: with a `tmcd`
  track, the 1st fragment is written without a header → corrupted file); `timecodestamper` (only accepts
  system memory: the NVMM→RAM→NVMM round trip takes CPU from 14 % to 46 % of a core per camera).
- **Dynamic recording branch** (implemented, step 2): the capture pipeline (`v4l2src` → `nvv4l2decoder` →
  `videorate` → `nvvidconv` → `tee allow-not-linked=true`) runs as long as the camera is plugged in; the
  recording branch (`queue` → `nvv4l2h264enc` → `h264parse` + timecode probe → `qtmux` → `filesink`) is
  attached to the tee while the recording gate is open and detached with an EOS (IDLE probe on the tee
  pad) when it closes, so that qtmux finalizes the file while the capture — and later the LiveKit branch —
  keeps running. The branch is linked **without caps check** (`PadLinkCheck::empty()`): `nvv4l2decoder`
  answers caps queries coming back up through the tee with NULL caps (link refused with `NOFORMAT`),
  negotiation then happens normally through the caps event. An error inside the branch (disk write…)
  drops only the branch, retried after 5 s. No buffer for 5 s → capture considered stuck, restarted.
  `filesink buffer-mode=2` (unbuffered: data goes straight to the page cache).
- **Rotation** (decision of 2026-09-28: right at capture): applied by the `nvvidconv` already present
  before the `tee` (`flip-method`, hardware VIC): no extra pass, zero CPU cost, recording and stream both
  upright. Changing the rotation during a session starts a new file (like a reconnection), because a
  90°/270° rotation changes the dimensions and qtmux cannot change dimensions mid-file.

### Hot-plug detection and resolution/fps selection

Cameras and microphones are **detected automatically, including hot-plugged ones**: plug-in → parameter
selection → recording + streaming; unplug → clean finalization of the files and unpublishing of the
tracks. No device needs to be declared in advance (`devices.yml` only contains overrides, §8).

- **Detection through udev** (implemented, step 2), not `GstDeviceMonitor`: PipeWire, running in the
  desktop session, hides GStreamer's V4L2/ALSA providers, and the ALSA provider does not report hot-plug.
  udev events (subsystems `video4linux` and `sound`) arrive once the `by-id`/`by-path` links exist. Kept:
  USB devices only (`ID_BUS=usb`), V4L2 nodes with the `:capture:` capability (not the metadata nodes),
  one ALSA card per `controlC*` device (opened as `hw:CARD=<id>,DEV=0`). The watcher runs on a dedicated
  thread; on (re)start it asks the device manager to re-enumerate, so no event is lost.
- **USB port** = `ID_PATH` without the trailing `:<config>.<interface>` (e.g.
  `platform-3610000.usb-usb-0:2.1`): the video and audio interfaces of a camera share it.
- **Automatic name**: `cam-<model>` / `mic-<model>` from the USB model name (`ID_MODEL`), e.g.
  `cam-hd-usb-camera`, `mic-usb-audio-device`; numeric suffix on duplicates; names set by overrides are
  reserved. The detection log gives `usb_path` and `by_id`, to copy into `devices.yml`.
- **Device manager**: one supervised task per device (`camera:<name>`, `mic:<name>`, policy "restart on
  error, stop when unplugged"); a `devices.yml` change restarts only the devices whose effective
  configuration changed (old task finalizes its files first).

Configured global cap (e.g. `1920x1080@60`). For each camera, its real MJPEG modes are listed and we pick:

1. the **largest resolution** ≤ cap,
2. then the **highest fps** available at that resolution, ≤ cap.

(E.g. cap 1080p60, camera with 1080p30 + 720p60 → 1080p30.) No compatible mode → camera ignored with a
clear log explaining why. If opening fails for lack of USB bandwidth (`No space left on device` / `Failed
to allocate`), we step down to the next mode (fps, then resolution) instead of looping on the error, with a
`warn` log.

Microphones: native microphone format (48 kHz preferred), no conversion; a camera with a built-in
microphone yields two independent sources (video + audio). ALSA caps are approximate (the camera
microphone announces 24-bit mono and refuses it when opened): the program builds an ordered list of
candidate formats (requested depth, else highest; requested channels, else fewest; preferred rate, else
highest) and tries them until one starts. Only S16LE/S24LE/S32LE are recorded (as-is in the WAV).

### Audio

- One **Broadcast WAV (BWF)** file per microphone, 48 kHz PCM in the native microphone format (no
  conversion): `bext` chunk (`TimeReference` = samples since midnight of the 1st captured sample, same
  `base_time + PTS` computation as video) + `iXML` chunk (`TIMECODE_RATE` 30/1,
  `TIMESTAMP_SAMPLES_SINCE_MIDNIGHT`). Current microphones: microphone of the "USB Camera" = 24-bit
  **stereo only** (mono only in 16-bit); C-Media microphone = 16-bit mono.
- Written by the program itself (`appsink` → BWF writer), not `wavenc`: sizes rewritten in the header
  every second of audio, so a file cut by a power loss is readable up to the last second. Continuous
  recording can exceed 4 GB (~4 h in 24-bit stereo): a `JUNK` chunk reserved after the RIFF header turns
  the file into **RF64** (`ds64` chunk, EBU Tech 3306) when needed. The appsink always receives the
  samples; a new file opens when the recording gate opens, the current one is finalized when it closes.
- **Recording gate** = requested (command bus: startup, future button) && enough disk space; a dedicated
  task combines both and every device task follows it.

### Files

- **Layout**: `RECORDINGS_DIR/<YYYY-MM-DD>/<name>_<YYYY-MM-DDTHH-MM-SS>.mov|.wav|.csv` (one directory per
  day).
- **Naming**: device `name` (`devices.yml` override or automatic name derived from the by-id), never
  `/dev/videoX`.
- **Startup**: recording starts as soon as the program starts (hence at boot) and runs continuously.
  **Later**: a button in the car will start/stop local recording (hardware not there yet) → provide right
  now an internal "start/stop recording" command that the button (and possibly remote control, §6) will
  trigger. Streaming is not affected by the button.
- **Continuous**: a single file as long as the device stays connected, no periodic splitting.
- **Disconnection/reconnection**: **new file** per connection session.
- **Keep everything**: no automatic purge.
- **Critical disk space**: below a threshold (`DISK_CRITICAL_GB`, 5 GB by default), clean finalization of
  the files and stop of local recording, error log; LiveKit streaming may continue. Rough budget:
  3 cameras × 12 Mbps ≈ 16 GB/h (a bit more if the 1080p60 camera uses a higher bitrate) → ~8 h on the
  current free space, ~25 h with the future 500 GB SSD. Write throughput (~2 MB/s) is negligible for an
  NVMe drive.

## 5b. Telemetry

Four sources, each in its own supervised task, with the same dual output: **local CSV** (source of truth)
+ **LiveKit publication** in the room metadata, one section per source (§6). Sampling periods configurable
in `.env` (`GPS_INTERVAL_MS`, `MODEM_INTERVAL_MS`, `UPS_INTERVAL_MS`, `SYSTEM_INTERVAL_MS`).

| Source | Access | Default period | CSV columns |
|---|---|---|---|
| `gps` | Modem GNSS, NMEA read by the program on the port given by ModemManager (`ttyUSB1` today) | 1 s | `timestamp`, `fix` (`none`/`2d`/`3d`), `lat`, `lon`, `alt_m`, `speed_kmh`, `course_deg`, `satellites`, `hdop`, `gps_time` |
| `modem` | ModemManager over D-Bus (no direct access to the AT ports, which ModemManager owns) | 5 s | `timestamp`, `state`, `access_tech`, `operator`, `signal_quality`, `lte_rssi_dbm`, `lte_rsrp_dbm`, `lte_rsrq_db`, `lte_sinr_db`, `nr_rsrp_dbm`, `nr_rsrq_db`, `nr_sinr_db`, `cell_id`, `tac`, `ip_connected` |
| `ups` | INA219 via `i2cdev`, bus 7, `0x41` | 2 s | `timestamp`, `load_voltage_v`, `current_a`, `power_w`, `percent` |
| `system` (decision of 2026-09-28) | `/sys/class/thermal`, `/proc/stat`, `/proc/meminfo`, GPU `load`, NVENC devfreq clock, `statvfs`, nvpmodel status, internal state | 5 s | `timestamp`, `cpu_temp_c`, `gpu_temp_c`, `tj_temp_c`, `cpu_load_pct`, `gpu_load_pct`, `ram_used_mb`, `nvenc_mhz`, `disk_free_gb`, `recording`, `cameras`, `mics`, `livekit_connected`, `power_mode` |

Implemented in step 3. Notes on the columns:
- `modem`: 5G NSA gives both the LTE anchor and the NR carrier, hence two sets of signal values. The
  modem reports unknown values as sentinels (-32768 dBm, -3276.8 dB): values outside physical ranges are
  left empty. `cell_id` / `tac` are hexadecimal, as reported. The extended signal values need
  `Signal.Setup` (rate = sampling period), requested by the program.
- `system`: NVENC utilisation is not exposed without root; its devfreq clock (`nvenc_mhz`, scaled by the
  load governor) is the proxy. `cpu_load_pct` is the whole machine (all cores). `livekit_connected` stays
  empty until the LiveKit step. `tj_temp_c` = junction (hottest) temperature.

- **CSV**: `RECORDINGS_DIR/<YYYY-MM-DD>/<source>_<YYYY-MM-DDTHH-MM-SS>.csv`, header on the 1st line, `,`
  separator, decimal point, `timestamp` in ISO 8601 UTC with milliseconds (same clock as the A/V timecode,
  §7). Empty field when a value is unavailable (e.g. no GPS fix), never a skipped line. Written line by
  line with frequent flushes (at most a few seconds lost on a power cut). New file on every (re)start of
  the source, as for video. Subject to the same recording button (coming later) and the same disk
  threshold as A/V.
- **UPS** — reproduce the reference script exactly: write the calibration (`0x05` = 26868) then the config
  (`0x00`: 16 V range, gain /2 80 mV, 12-bit ADC 32 samples, continuous mode) on initialization **and after
  any I2C error** (the INA219 reverts to its default config if it loses power); bus voltage =
  `(reg02 >> 3) × 4 mV`, current = `signed reg04 × 0.1524 mA`, power = `reg03 × 3.048 mW`, percentage =
  `(V − 9) / 3.6 × 100` clamped to [0, 100] (3S pack). Big-endian I2C registers. Read on 2026-09-28:
  12.49 V (config already applied). **No shutdown threshold**: the program is resilient to power cuts by
  design (fragmented files, frequent flushes); the UPS is only a telemetry source (logs + LiveKit).
- **GPS** (implementation decision of 2026-09-28: **the program reads the NMEA sentences itself, no
  gpsd**). The NMEA port can only have one reader and the program needs the data anyway; the chrony time
  fallback will be fed by the program through a chrony SOCK refclock (§7), one daemon less. The program
  enables the GNSS through ModemManager in `gps-unmanaged` mode (the NMEA port stays free; `Location.Setup`,
  re-applied if the modem forgets it), gets the NMEA port from ModemManager's port list (type GPS, robust
  to `ttyUSB` renumbering), opens it in raw mode (no echo back to the modem) and parses RMC/GGA/GSA from any
  talker. A fix older than 3 s counts as no fix; no sentence for 10 s → port reopened. Logs: only a fix
  gained or lost, once the new state has lasted 10 s (a weak sky view made the fix flap: 1355 lines in one
  night); 2D ↔ 3D changes are only in the CSV. A **GNSS antenna**
  is connected to the modem (indoors on 2026-09-28: sentences received, no fix). **GPS is optional**: no fix, GNSS impossible to enable
  or NMEA port missing = normal state (tunnel, garage, indoors) → `info` log on state change (not on every
  reading), CSV lines with empty fields, new enabling attempt with backoff; no other feature depends on it
  (time falls back to NTP/RTC).
- **Modem**: distinguish "no network" (normal, `info`/`warn`) from "modem missing/failed" (§9).

## 6. LiveKit streaming

- **Default**: **AV1**, **every camera streams with the same settings** — 540p30 @ 1.2 Mbps
  (`STREAM_VIDEO_*` in `.env`). No notion of main camera in `.env`.
- **Main camera** (decision of 2026-09-28): **optional parameter, set only when configuring a camera
  through LiveKit** (admin page, stored in `devices.yml`). As long as none is designated, or if the
  designated camera is unplugged, all cameras are treated equally. A camera designated as main gets:
  1. **higher streaming quality**: its own streaming resolution/fps/bitrate, set at the same time
     (`devices.yml` overrides);
  2. **priority on degraded 5G** (implemented on 2026-09-29): WebRTC `bitrate_priority` 4.0 on its sender
     (1.0 for the other tracks). When the uplink is short, the main camera keeps most of it and the others
     give way, **no stream is stopped** (the automatic pause of the secondary cameras, built then removed on
     2026-09-28, stays out). Details: "Prioritizing a stream" below;
  3. **highlighting in the front end**: participant attribute `main_camera=<track name>` (absent without a
     main camera: LiveKit deletes an attribute set to an empty string) and `car.main_camera` in the room metadata.
  The camera is identified in `devices.yml` by its **physical USB port** (`/dev/v4l/by-path`), reliable with
  fixed wiring and valid offline once saved.
- **Prioritizing a stream** (study of 2026-09-28; `degradation_preference` and RTP priority implemented):
  - `degradation_preference` exists in the Rust SDK (`TrackPublishOptions`, default `MaintainFramerate`
    for cameras). With the SDK's Jetson encoder it has little effect on bandwidth: the encoder declares
    `scaling_settings = kOff` and `is_hardware_accelerated = true`, so WebRTC never lowers the resolution
    because of bandwidth (no QP-based quality scaler); under congestion it lowers the encoder's target
    bitrate and drops frames. It mainly governs the reaction to CPU overuse, where `MaintainFramerate`
    would downscale the DMA-BUF frames (CPU copy path, like the one behind the mute crash): **set to
    `MaintainResolution`** (decision of 2026-09-28): frames are dropped rather than downscaled.
  - **RTP priority — implemented on 2026-09-29** for the main camera. libwebrtc's bitrate allocator gives
    every stream of the (single) publisher connection its minimum, then splits the rest in proportion to
    each sender's `bitrate_priority`, up to each stream's maximum. The LiveKit Rust SDK did not allow it:
    the `libwebrtc` crate reset `bitrate_priority` to 1.0 when converting to C++ (and dropped it the other
    way; its `priority` field is the DSCP `network_priority`, ignored by mobile networks and the
    Internet), and `livekit` keeps the track's `RtpTransceiver` private. **SDK patch** (`vendor/`, wired by
    `[patch.crates-io]`, diff in `vendor/sdk.patch`): `bitrate_priority` field carried both ways (5
    lines), `LocalVideoTrack::transceiver()` public (1 line); `webrtc-sys` (C++) unchanged. The room task
    sets 4.0 on the main camera's sender right after publishing and reads it back (log `main camera
    bitrate priority set`); a change of main camera restarts both cameras, which are republished with
    their new priority. The SDK's own `set_parameters` calls (degradation preference, dynacast) read then
    write the parameters, so the value survives them.
    Measured (§12): uplink capped at 1.5 Mbps, 2 cameras + 2 microphones → without a main camera
    ~525/550 kbps each; with a main camera ~850 kbps for it and ~240 kbps for the other (same total, 30 fps
    and 0 loss on both, audio untouched at 64 kbps).
  - Static: lower bitrate/frame rate for the secondary cameras in `devices.yml`, higher for the main one.
  - Dynacast (`RoomOptions::dynacast`): the SFU tells the car which tracks nobody watches and the SDK
    stops encoding them — saves uplink and energy if the front end only subscribes to what it shows. To
    test with single-layer AV1 tracks.
- **AV1 codec — decision of 2026-09-25**: priority to transmission quality under constrained bandwidth (5G)
  over viewer compatibility. Accepted: browsers/devices without AV1 decoding (Safari without a hardware AV1
  decoder, old devices) will not be able to play the streams. No backup codec (a backup codec would be
  encoded in software VP8). Local recording stays H.264 (§5).
- **Rust `livekit` SDK: Jetson hardware encoding validated** (2026-09-25, see §12). `webrtc-sys` embeds a
  Jetson MMAPI (NVENC) encoder, built automatically on aarch64 if `/usr/src/jetson_multimedia_api` exists
  (package `nvidia-l4t-jetson-multimedia-api`), preferred in `VideoEncoderBackend::Auto` mode.
  **Mandatory** settings in `TrackPublishOptions`:
  - `video_codec: VideoCodec::AV1` (chosen; `H264` works too) — the default is **VP8 → software libvpx**,
    and `H265` is not negotiated by the server → silent fallback to software VP8;
  - `simulcast: false` — the Jetson encoder does not support simulcast (`supports_simulcast = false`):
    WebRTC would create one encoder per layer with CPU downscaling;
  - `video_encoder: Auto` (or `Hardware`).
  Limits of the SDK's Jetson encoder: AV1 profile 0 in NV12, single tile; H.264 Constrained Baseline level
  3.1 (≤ 720p30); CBR and ultrafast preset in both cases, not configurable.
- **Build** (versions and workarounds):
  - clang ≥ 21 required (libwebrtc's hermetic libc++): clang 23.1.2 in `~/src/LLVM-23.1.2-Linux-ARM64`,
    through `CC`/`CXX`.
  - JetPack 7: the R39 MMAPI `NvVideoEncoder.cpp` calls `NvBufSurfaceGetDeviceInfo`, which is missing from
    the SDK's lazy-loading stubs → explicit linking
    `-L/usr/lib/aarch64-linux-gnu/nvidia -Wl,--no-as-needed -lnvbufsurface`.
  - Versions: **`livekit` 0.9.2 (2026-09-23) is broken** (sub-crates `livekit-signaling` 0.1.3 /
    `livekit-data-stream` 0.1.6 / `livekit-api` 0.8.0 on prost 0.14 vs `livekit-protocol` 0.7.13 on
    prost 0.12). Consistent set: `livekit` =0.9.1, `livekit-api` =0.7.1, `livekit-data-stream` =0.1.5,
    `livekit-signaling` =0.1.2, `livekit-token` =0.1.2, `libwebrtc` =0.3.48, `webrtc-sys` =0.3.45, and
    **`livekit-common` =0.1.3** (0.1.4 pulls `livekit-protocol` 0.8: `livekit` 0.9.1 no longer compiles).
  - In the project: `.cargo/config.toml` sets `CC`/`CXX` and the link flags; the pinned set is in
    `Cargo.toml`.
- **Feeding path: DMA-BUF, zero copy** (decision of 2026-09-28, validated). The camera's stream branch
  (tee → `queue leaky=downstream` → `videorate drop-only=true max-rate=<fps>` → `nvvidconv
  output-buffers=10` downscale → `appsink`) hands each NVMM surface to the SDK as a DMA-BUF file descriptor
  (`NativeBuffer::from_dmabuf`, fd read from the `NvBufSurface` behind the buffer; struct offsets checked
  at compile time) → `NativeVideoSource` → SDK Jetson AV1 encoder. No CPU copy; the SDK handles keyframe
  requests (PLI, late subscribers) and congestion control natively. The last 4 surfaces are kept alive
  while the encoder reads them asynchronously. Stream size = source aspect ratio fitted in
  `STREAM_VIDEO_RESOLUTION` (portrait for a rotated camera), never upscaled.
  - **Decoder surfaces**: NVMM buffers downstream keep their source decoder surface referenced; with the
    driver default (`num-extra-surfaces=1`), holding 4 stream frames starves `nvv4l2decoder` and freezes the
    capture. The capture pipeline sets `num-extra-surfaces=10` (~30 MB of NVMM per camera).
  - Rejected — *raw* path (`nvvidconv` to system I420 → `I420Buffer` → SDK): double copy, 23 % CPU per
    stream in the real test (§12). Rejected — *pre-encoded* path (`nvv4l2av1enc` → `capture_encoded_frame`):
    rate control works (the SDK relays WebRTC's target bitrate), but NVIDIA's GStreamer encoders on JetPack
    7 ignore force-key-unit events (H.264 and AV1): late subscribers never get a keyframe (tested
    2026-09-28, §12).
  WHIP/Ingress is no longer needed.
- **Signaling over IPv4**: the SDK's WebSocket connection is not configurable (no custom resolver or
  transport), so the "happy eyeballs" fallback cannot live in the program. Workaround until the Livebox
  IPv6 443 issue is fixed: prefer IPv4 system-wide in `/etc/gai.conf` (`precedence ::ffff:0:0/96 100`),
  step 5. The media (ICE, UDP) is not affected.
- **Room and identity**: `LIVEKIT_ROOM` and `LIVEKIT_IDENTITY` in the environment. Tokens generated locally
  from `LIVEKIT_API_KEY`/`LIVEKIT_API_SECRET`.
- **Tracks**: one video track per camera and one audio track per microphone, named after the device
  `name` (e.g. `cam-front`, `mic-driver`). Audio: Opus (software encoding, negligible cost), DTX and RED
  off; the microphone's appsink callback converts the samples to 16-bit and hands them over with a
  non-blocking `try_send` to a task that feeds `NativeAudioSource` in 10 ms frames — streaming can never
  slow down the recording, and it keeps running while local recording is stopped.
- **Implementation (step 4)**: devices register their source in a `StreamHub`; the `livekit` task
  (supervised, backoff capped at 15 s so that streaming comes back quickly) connects, publishes every
  registered source, and tells each device through its registration whether its track is published — the
  camera attaches its stream branch only then (no VIC/NVENC work while offline). It also checks the
  `encoderImplementation` stat once per track (`error` log if not "Jetson"). Tracks are unpublished when
  a device goes away. `auto_subscribe` off (the car never subscribes).
- **Telemetry: only in the room metadata** (decision of 2026-09-28). No data packets and no data tracks:
  the front end listens to a single event (`RoomMetadataChanged`) and gets the full state as soon as it
  joins (included in the room info on join), including when the Jetson is offline.
  - Content: a single JSON `{ "v": 1, "ts": …, "gps": {…}, "modem": {…}, "ups": {…}, "system": {…} }`, one
    section per source (same fields as the CSV, with its own reading `ts`), plus the car state (recording,
    cameras/microphones present, main camera). Schema in `docs/PROTOCOL.md`.
  - Writing: HTTPS request to the server API (`RoomClient::update_room_metadata`, Twirp), not through the
    WebRTC connection. **Coalescing**: at most one request in flight, 1 Hz max rate (configurable); if a
    request is slow or fails (degraded 5G), intermediate values are overwritten by the most recent one,
    never queued; 10 s timeout per request, retry every 10 s while failing; the room is (re)created if the
    server does not know it. Same IPv4 preference as signaling (Orange proxy, IPv6 443 blocked). Fed by
    the telemetry sources: every CSV sample also updates its section, whether local recording runs or not.
  - Room created by the Jetson (`create_room`, idempotent) with a long `departure_timeout`, so that the
    state survives the Jetson's absence. Front end: "last updated X ago" based on `ts`. `create_room`
    does **not** change the timeouts of a room that already exists (tested on the server on 2026-10-05).
    If a viewer's join creates the room (car absent for more than 24 h), that room would get the server's
    default timeouts. Viewer tokens therefore carry the same room configuration (`roomConfig`, 24 h
    timeouts, `docs/PROTOCOL.md`).
  - System designed for **a single car** (one Jetson per room): no write conflicts.
  - Bandwidth: **a single request per update on the Jetson side**, whatever the number of viewers; the
    fan-out to each viewer is done by the server (homelab upstream bandwidth). The Jetson receives the
    fan-out too (~1–2 KB/s downstream on 5G, negligible).
  - Accepted costs: TCP path (latency + head-of-line blocking on degraded 5G, a few hundred ms, irrelevant
    at ≤ 1 Hz); the full state (~1–2 KB) is re-sent to every viewer on every update, over the signaling
    channel.
  - Limits: state lost when the LiveKit server restarts (without Redis) until the Jetson reconnects; no
    history (GPS track, charts) → later, a front-end backend subscribed to the room.
  - Future high-frequency data (CAN bus, phase 2): not suited to metadata → data tracks at that point. The
    publication module is isolated behind an interface so that this channel can be added without
    touching the sources.
- **Remote control / admin page** (decision of 2026-09-28: **prepared from the 1st version, coded later**).
  Everything goes through LiveKit, so no inbound port on the Jetson:
  - RPC (`register_rpc_method`) for commands with an acknowledgement; participant attributes
    (`set_attributes`) to publish the applied state; commands accepted only from an authorized identity
    ("control room" token with data permission, viewers without it).
  - Without Internet, the page shows "offline"; the last applied configuration is persisted locally and
    stays valid after a restart.
  - To build from the 1st version: **internal command bus** (button, config reload and future RPC all end
    up there) and **safe configuration application** (§8).
  - Out of reach of remote control: `.env` (URL, keys, room, identity), so that a remote mistake can never
    cut access to the car.

### Protocol towards the front end

The front end will be entirely rewritten: **this project defines the formats** (decision of 2026-09-28).
The contract is documented in **`docs/PROTOCOL.md`**: room and participant, tracks, the `main_camera`
participant attribute, versioned JSON schema of the room metadata (`v` field: telemetry + car state), and
later the RPC methods of the admin page.

## 7. Time reference

Consistent timestamps across video, audio, logs and telemetry:

- **NTP** over 5G when available (already active),
- **GPS** from the modem as a fallback when there is no network: the program (single NMEA reader, §5b)
  feeds chrony through a SOCK refclock. The modem provides no PPS: accuracy of a few tens of ms once the
  NMEA latency is compensated, good enough as a fallback,
- the Jetson's **RTC** at boot (kept in sync by chrony's `rtcsync`).

Implementation (step 5):
- Each valid RMC sentence (status `A`) → one `sock_sample` datagram to `/run/chrony.racecast.sock`:
  `tv` = local time when the line was read, `offset` = GNSS time − `tv`. GNSS dates before 2026 are
  rejected (week-number rollover bug of some receivers). Non-blocking socket; chrony missing or not
  configured = samples dropped, `info` log on change.
- chrony (`deploy/chrony.conf`): `refclock SOCK … refid GPS delay 0.5 offset 0.020`. The large delay gives
  the GPS a root distance ≥ 0.25 s: NTP servers (a few tens of ms) are always selected and never averaged
  with it (`combinelimit` 3); the GPS is only selected when no server is reachable. `offset` compensates
  the NMEA latency (sentence emitted after the fix epoch): **+20 ms, std dev 0.75 ms** measured against NTP
  on 2026-09-28 (§12).
- chronyd binds the socket as root with mode 0755 (umask 022): a drop-in (`deploy/chrony-override.conf`)
  hands it to the `jetson` group once chronyd is ready. The path matches the `@{run}/chrony.*.sock` rule of
  the chronyd AppArmor profile shipped by Ubuntu.
- Clock steps: Ubuntu's default `makestep 1 3` only steps during the first 3 updates after chronyd starts
  (then slews). A step at boot while recording shifts the next frames' timestamps (repeated or dropped
  frames by `videorate`); small in practice, since the RTC keeps the time across reboots.

The timecode of the A/V files derives from this clock (time of day).

## 8. Configuration

- **`.env`** (ignored by git, documented by `.env.example`): `LIVEKIT_URL`, `LIVEKIT_API_KEY`,
  `LIVEKIT_API_SECRET`, `LIVEKIT_ROOM`, `LIVEKIT_IDENTITY`, `RECORDINGS_DIR`, log path and level, critical
  disk threshold…
- **Defaults in `.env`** (full list in `.env.example`): capture cap (`VIDEO_MAX_RESOLUTION`,
  `VIDEO_MAX_FPS`), local bitrate (`VIDEO_BITRATE_KBPS`), streaming settings shared by all cameras
  (`STREAM_VIDEO_RESOLUTION`, `STREAM_VIDEO_FPS`, `STREAM_VIDEO_BITRATE_KBPS`), microphones
  (`AUDIO_SAMPLE_RATE`, `STREAM_AUDIO_*`), streaming of new devices by default (`STREAM_VIDEO_ENABLED`,
  `STREAM_AUDIO_ENABLED`), telemetry periods (`GPS_INTERVAL_MS`, `MODEM_INTERVAL_MS`, `UPS_INTERVAL_MS`,
  `SYSTEM_INTERVAL_MS`), UPS (`UPS_I2C_BUS`, `UPS_I2C_ADDRESS`). An invalid value is replaced by the
  program's built-in default, with a `warn` log, never a shutdown.
- **Paths** (`RECORDINGS_DIR`, `LOG_DIR`, `DEVICES_FILE`): absolute or **relative to the directory that
  contains the `.env`** (not to the current directory, which depends on how the program is launched:
  systemd, SSH, cargo run). A leading `~` expands to `$HOME`. The resolved path is logged at startup; a
  missing directory is created, a non-writable directory → `error` log and restart of the task concerned,
  never a shutdown.
- **`devices.yml`** (`DEVICES_FILE`, default `./devices.yml`): **empty at first**, created by the program if
  missing. It only contains **per-device overrides**, added as each camera/microphone gets configured
  (later from the admin page through LiveKit, by hand until then). Every detected device works without an
  entry, with the `.env` defaults.
  - Device identified by **USB port** (`by-path`) or by **by-id**; the port wins (the only way to tell apart
    two identical devices without a serial number).
  - Overridable fields: `name`, `rotation`, bitrates, streaming on/off, main camera; microphone: format
    (bits, channels).
  - Device without an override: automatic name derived from its by-id (e.g. `hd-usb-camera`), numeric
    suffix on duplicates. `name` is used for file names, log tags and LiveKit track names.
  - Schema (implemented, step 1): `cameras` and `microphones` lists. Camera: `usb_path` **or** `by_id`,
    `name`, `rotation` (0/90/180/270), `main` (bool), `video_bitrate_kbps`, `stream` (`enabled`,
    `resolution`, `fps`, `bitrate_kbps`). Microphone: `usb_path` **or** `by_id`, `name`, `channels`,
    `bit_depth` (16/24/32), `stream` (`enabled`, `bitrate_kbps`). Unknown fields rejected (typos), names
    unique across all sources, same bounds as in `.env`. `usb_path` = `by-path` prefix of the physical
    port (e.g. `platform-3610000.usb-usb-0:2.1`). File created with a commented template;
    `racecast-emitter --check-config` validates it without starting the program.
  - Updates: only by the program (admin page through LiveKit, which applies the new configuration in
    memory and then writes it); the file is not watched. After a manual edit (until the page exists):
    `SIGHUP` / `systemctl reload racecast`. The last **confirmed** version is copied to
    `devices.yml.last-good`, used at startup if `devices.yml` is invalid or not confirmed yet (see below).
- **Safe configuration application** (admin page, or `SIGHUP` after a manual edit):
  1. full validation (types, bounds, consistency) before anything is applied; rejection = precise error
     message, nothing changes;
  2. device-by-device application: only the devices whose effective configuration changes are restarted,
     the others are never touched;
  3. automatic confirmation: a configuration is only "good" if the restarted devices produce frames for a
     few seconds, otherwise automatic rollback to the last valid configuration;
  4. atomic write (temporary file + `fsync` + `rename`) of the validated configuration.

  Implementation (2026-09-29): a change is a **transaction** run by the device manager
  (`capture::manager`), started by the command bus (`ReloadDevices`; the future RPC will use the same
  path):
  - the restarted devices must each deliver frames for **5 s** (`CONFIRM_WINDOW`) with frames still
    arriving at the end, without **any** failure (capture error or stall, recording or stream branch that
    fails to start or fails, task error), within **30 s** (`CONFIRM_TIMEOUT`). Each device task reports its
    start, frames and failures through a `capture::health::Health`;
  - **all or nothing**: the first failure, or the timeout, puts **every** device of the change back on its
    previous configuration (restarted again). A partial result is never kept: `devices.yml` must always
    describe what runs, and cross-device rules (unique names, a single main camera) cannot be checked
    device by device;
  - confirmed → the file becomes `devices.yml.last-good`. Rolled back → `devices.yml` is rewritten with the
    confirmed version (so that a restart does not apply the rejected one again; left alone if it was edited
    again meanwhile) and the rejected content is kept in `devices.yml.rejected`; `error` log with the
    reason;
  - one change at a time (a second `SIGHUP` during a confirmation is refused); the transaction runs aside
    from the other commands (the recording button is never held back); a device plugged in during a
    change starts with the new configuration and joins the change;
  - **at startup**, a valid `devices.yml` that differs from `devices.yml.last-good` (edited while the
    program was stopped, or a change interrupted by a restart) is not trusted: the devices start with the
    confirmed version, then the file is applied as a transaction (source `startup`);
  - without GStreamer (no device manager), a valid change is committed at once (nothing to confirm).

## 9. Resilience

1. **systemd** `Restart=always` + watchdog (`sd_notify`, `WatchdogSec=10`, pings every 5 s from the async
   runtime, a ping later than 1.5 periods is logged as `warn`): `deploy/racecast.service`, without
   dependency on `network-online.target` (starts at boot even without a network). A hung process is back
   up in ~13 s (10 s + `RestartSec=2` + startup); 30 s at first, lowered on 2026-09-28. Not lower: a false
   restart splits the recording and cuts the stream, and the pings share the runtime with everything else
   (the late-ping log shows how close it gets).
2. **Internal supervisor**: one task per stream, retry with backoff, without restarting the process.
3. **5G outages**: recording continues; publication detects the loss, retries with backoff, resumes
   without loss or duplication on the local side.
4. **5G modem**: supervision through ModemManager (D-Bus). "No network" = normal state, never any action.
   **Autonomous modem restart allowed with safeguards** (decision of 2026-09-28): only if the modem has
   failed (missing from ModemManager, stuck in the `failed` state, or connected but with no possible IP
   traffic for several minutes while the signal is good); at most once every 10 min; `warn` log. Cuts
   streaming and WireGuard SSH for ~30 s.
5. **Power cut**: no preventive shutdown on a UPS threshold. Resilience comes from the formats (fragmented
   `.mov`, repairable WAV headers, line-by-line CSV) and frequent flushes.

## 9b. WireGuard VPN (administration)

A WireGuard tunnel (`wg0`, set up outside this repository) gives SSH access to the Jetson for
administration. It is not part of the program, and the program does not depend on it.

- **Streaming does not go through the tunnel**: WebRTC media is already encrypted (DTLS-SRTP) and goes
  direct (UDP, ICE). The tunnel would add header overhead (~7 %), user-space crypto (CPU/energy), a single
  point of failure (tunnel restart = stream cut) and a reduced MTU. The tunnel only routes the private
  administration subnets (`Table = off`), so the program's traffic never uses it.
- To keep in mind: ICE must not pick a `wg0` candidate if the SFU announces a private address that is
  reachable through the tunnel.

## 9c. System setup (part of the project)

Decision of 2026-09-28: shipped with the program (files in the repository + installation procedure):
- **systemd service** `racecast.service`: starts at boot, `Restart=always`, `Type=notify` + `WatchdogSec=`,
  runs as the `jetson` user.
- **Time**: chrony instead of `systemd-timesyncd` (NTP first, modem GPS as fallback through a SOCK
  refclock fed by the program, §7). No gpsd.
- **Write-back**: `vm.dirty_expire_centisecs` / `vm.dirty_writeback_centisecs` lowered (~1 s) so that
  recorded data leaves the page cache quickly (a `kill -9` loses nothing, a power cut would otherwise lose
  up to ~30 s of cached data).
- **Permissions**: I2C, video, audio and dialout (NMEA port) groups, and a polkit rule for the ModemManager
  actions the service needs without a desktop session: `Location.Setup` (GNSS), `Location.GetLocation`
  (cell id), `Signal.Setup` (extended signal values) and `Modem.Reset` (restart safeguard). From the
  desktop session they are already allowed (tested on 2026-09-28); without the rule, the program logs a
  warning once and leaves the corresponding fields empty.
- **IPv4 preference** (`precedence ::ffff:0:0/96 100` in `/etc/gai.conf`): **not applied** (decision of
  2026-09-28): the user fixes the IPv6 connectivity of the server instead (§11). Until then, real-server
  tests go over Wi-Fi.
- **Power mode**: **15W** (decision of 2026-09-28, measured with 2 cameras, §12). A mode only caps the
  clocks; DVFS sets them from the load. Under the full load the hardware already runs below the 10W caps
  (cores at 729 MHz most of the time with short peaks, EMC at 2133 MHz; NVENC/NVDEC/VIC are not capped by
  any mode), and **10W was measured at the same consumption** (6.65 W vs 6.62 W): 15W costs nothing and
  keeps CPU headroom for the 1080p60 camera. 20W and MAXN only add 2 cores. To re-measure with the 3
  cameras (§11).

Implementation (step 5): files in `deploy/`, installed by **`sudo deploy/install.sh`** (idempotent); the
full reinstall procedure (packages, Rust, clang 23, backups before wiping) is `deploy/INSTALL.md`:

| File | Installed as |
|---|---|
| `racecast.service` | `/etc/systemd/system/racecast.service` (enabled and restarted) |
| `chrony.conf` | `/etc/chrony/conf.d/racecast.conf` (`apt install chrony` replaces `systemd-timesyncd`) |
| `chrony-override.conf` | `/etc/systemd/system/chrony.service.d/racecast.conf` (socket permissions, §7) |
| `polkit.rules` | `/etc/polkit-1/rules.d/50-racecast.rules` |
| `sysctl.conf` | `/etc/sysctl.d/90-racecast.conf` (`vm.dirty_expire_centisecs` = `vm.dirty_writeback_centisecs` = 100) |

- Groups: `jetson` added to `video`, `render`, `audio`, `i2c`, `dialout` (already the case on this machine;
  the service also lists them in `SupplementaryGroups=`).
- polkit: the ModemManager policy maps `Location.Setup`/`GetLocation` to the
  `org.freedesktop.ModemManager1.Location` action and `Signal.Setup`/`Modem.Reset` to
  `org.freedesktop.ModemManager1.Device.Control`, both `allow_active` only: a system service has no session,
  so the rule allows these two actions to the `jetson` user.
- The script checks the power mode but does not change it (a mode change may ask for a reboot
  interactively).

## 10. Logging & observability

First-class requirement: understand afterwards what happened, without interactive access.

- `tracing` + `tracing-subscriber`, structured **JSON** format.
- Local file with daily rotation, frequent flushes to survive a hard power cut. **Keep everything**
  (decision of 2026-09-28), like the recordings: no deletion.
- Timestamps from the reference clock (§7), cross-checkable with video and telemetry.
- Tag per subsystem: `camera:<name>`, `mic:<name>`, `gps`, `modem`, `ups`, `system`, `livekit`, `storage`,
  `can` (phase 2).
- Clear levels: expected 5G disconnections/reconnections = `info`/`warn`, not `error`.
- The logger must never panic or block (full disk → best-effort degradation).
- `std::panic::set_hook`: every panic is logged before the restart.
- Remote upload deprioritized: errors only, rate-limited, never competing with the LiveKit tracks — or
  even deferred until back on Wi-Fi.

## 11. Open points

To be validated in practice, by priority:

1. **Timecode sync in DaVinci Resolve Studio**: import and playback of the `.mov` files (finalized and
   interrupted) ✅ validated by the user on 2026-09-25. Multi-source sync to validate with the
   `~/racecast-tests/sync/` set (2 cameras + 2 simultaneous BWF microphones, constant 30 fps).
   2026-10-05: the timeline sync of Resolve is not offered, because the `.mov` files have no audio. The
   timecodes are consistent (both `.mov` files start at 15:24:18:12, the BWF files at 15:24:18.438 and
   .440 on 2026-09-29). Next: the user tries the timecode-based tools (Media Pool *Auto Sync Audio > Based
   on Timecode*, multicam clip with *Angle Sync: Timecode*). **Fallback decided**: every `.mov` gets an
   audio track that is always present and **silent**, whether or not the camera has a microphone. The real
   sound stays in the BWF files. It would come from a silent source inside the video pipeline, with no link
   to the microphone tasks.
2. **LiveKit over 5G**: the streaming is implemented and validated on the LAN and on the real server over
   Wi-Fi (§12). **Blocking over 5G**: inbound IPv6 TCP 443 to the reverse-proxy host is dropped by the
   Livebox (§12). Fix: open TCP 443 over IPv6 to that host in the box's IPv6 firewall (or remove the
   server's AAAA record), in progress by the user (the IPv4
   preference of §9c is deliberately not applied; real-server tests go over Wi-Fi meanwhile).
   Then test over 5G: behavior of the streams on a degraded link (need for a main-camera priority?),
   reconnection after a network cut, keyframe bursts when viewers join.
3. **NVENC and disk load**: 3 H.264 (one 1080p60) + 3 AV1 540p30 + 3 MJPEG decodes, and simultaneous
   writing of 3 videos + audio + CSV on the NVMe (should be enough, ~2 MB/s) — to validate once the 3rd
   camera is there.
4. **GPS outdoors**: get a fix with the car outside and check the CSV and the metadata (position, speed,
   `gps_time`) while driving (speed, course). A 3D fix was already obtained on 2026-09-28 with the car
   parked (4–5 satellites, HDOP 1.6) and the chrony refclock calibrated from it (§12); to re-calibrate:
   with the clock synchronized over NTP, read the offset of `GPS` in `chronyc sourcestats` (positive =
   sentences late) and put it in seconds as `offset` in `deploy/chrony.conf` (chrony adds it to every
   sample), then re-run `deploy/install.sh`. Modem restart
   safeguard not exercised (needs a modem stuck in `failed`).
5. **Room metadata**: 1 Hz validated with one viewer (1.2 KB); check with several viewers.
6. Final local bitrate (12 Mbps VBR for now, to revisit for the 1080p60 camera).
7. **USB 2 bandwidth**: all cameras + the modem share a 480 Mbit/s hub (§2) — test 3 simultaneous cameras
   and the `ENOSPC` fallback.
8. **Physical hot-plug test**: unplug/replug a camera and a microphone while recording and streaming (the
   code path is in place; not tested yet because it needs someone at the car). Also check that PipeWire
   (desktop session only, no linger) never holds a USB microphone when the service starts.
9. **Later**: recording button in the car; admin page (LiveKit RPC); CAN bus (phase 2).

## 12. Validation log

### 2026-09-25 — Container and timecode (HD USB camera, 1080p30 MJPEG)

| Test | Result |
|---|---|
| `isofmp4mux` (0.12.11 and main): timecode option | ❌ no `tmcd` written (the meta is only copied into the fragment headers) |
| Fragmented `mp4mux` + `timecodestamper` | ❌ no `tmcd` track |
| `qtmux` `dash-or-mss` + `timecodestamper` | ❌ corrupted file (1st fragment without a header); OK without timecode |
| `qtmux` `first-moov-then-finalise` + `timecodestamper` | ✅ `tmcd` read by ffprobe, finalized file OK |
| Same, `kill -9` at ~22 s | ✅ 21.18 s readable (630 decoded frames), timecode intact |
| Direct NVMM CPU (no timecode) | 14 % of a core |
| CPU with `timecodestamper` (NVMM→RAM detour) | 46 % of a core (×3.4) |
| Rust pad probe `VideoTimeCodeMeta` on H.264 buffers | ✅ `tmcd` = clock within 1 frame, 14 % CPU |
| Same, `kill -9` at ~8 s | ✅ 7.10 s readable (210 frames), timecode intact |

### 2026-09-25 — Encoding of the LiveKit branch (Rust SDK)

Local `livekit-server --dev` 1.13.7 server, synthetic 960×540@30 stream, 1.2 Mbps, simulcast off:

| Configuration | `encoderImplementation` (WebRTC stats) | Process CPU | NVENC0 (tegrastats) |
|---|---|---|---|
| H.264, `Auto` backend | **Jetson MMAPI H264 Encoder** | **8 %** | active (≤ 30 %) |
| H.264, `Software` backend | OpenH264 | 63 % | off |
| VP8 (SDK default), `Auto` | libvpx | 57 % | off |
| **AV1**, `Auto` | **Jetson MMAPI AV1 Encoder** | **7 %** | active (≤ 35 %) |
| H.265, `Auto` | libvpx (VP8 fallback, reduced resolution) | 35 % | off |

AV1: hardware encoding OK (Orin NVENC, NV12, CBR, single tile). On the viewer side, AV1 decoding is required
(Chrome/Firefox/Edge OK; Safari only on hardware with an AV1 decoder). H.265: not negotiated with the
server by default → silent fallback to software VP8 (avoid / monitor).

### 2026-09-25 — Sync test set (`~/racecast-tests/sync/`)

A single pipeline (real-time clock), 30 s: `cam-hd.mov` (TC 13:32:02:04), `cam-generic.mov`
(TC 13:32:01:13), `mic-camera.wav` and `mic-usb.wav` (BWF, TC 13:32:01:08). Video at constant 30 fps
(ffprobe: 30 fps / 30 tbr, versus 29.59 fps / 250 tbr before `videorate`). Realistic start offset between
cameras (~0.7 s of initialization), whereas without `skip-to-first=true` all sources showed the same start
time.

### 2026-09-25 — AV1 streaming to the real server (`wss://live.minarox.fr`, room `racecast-test`)

Real raw path: HD camera MJPEG 1080p30 → `nvv4l2decoder` → `nvvidconv` 960×540 I420 → `appsink` →
`NativeVideoSource` → SDK Jetson AV1 encoder. A 2nd participant on the Jetson subscribes through the
server. **Network: Wi-Fi, server on the same LAN (homelab)** — RTT 2–18 ms, not representative of 5G.

| Side | Result over 60 s |
|---|---|
| Publisher | `Jetson MMAPI AV1 Encoder`, 960×540, 23–27 fps, ~1,195 kbit/s sent (target 1,200), 0 % loss, no quality limitation, CPU 23 %, NVENC ≤ 38 % |
| Viewer (through the SFU) | codec `video/AV1`, decoded (dav1d), 960×540, 23–28 fps, ~1,195 kbit/s received, 0 packets lost, jitter 1–5 ms, 2 freezes at startup only (before the 1st keyframe) |

Notes: the 23–27 fps come from the camera (already 28 fps on average locally), not from the SDK. CPU at
23 % versus 8 % in the synthetic test: double copy (NVMM → system I420 by the VIC, then memcpy into the
NVENC buffers by the SDK) → an argument for the pre-encoded path.

**Over 5G (`wwan0`, Orange, IPv6): failure.** The TCP connection to `live.minarox.fr:443` is established
(~170 ms) but the server resets it right after the TLS ClientHello. HTTPS to other sites works over 5G:
the block is on the homelab side (filtering of non-LAN IPs at the reverse proxy, firewall or box level).

In-depth diagnosis (same day): a phone on 5G does reach `https://live.minarox.fr`. From the Jetson through
`wwan0`: plain HTTP (port 80) OK (308); **every** TLS negotiation is cut (RST ~1.7 s after the ClientHello),
whatever the SNI (live, homelab, unknown, none), the TLS version (1.2/1.3) or the ALPN. **Ruled out**: SNI
filtering, MTU/PMTU problem (same failure with MTU 1400/1340/1280 and reduced MSS), 5G blocking TLS in
general (Google/Cloudflare OK). The hop limit (62 for all inbound traffic, Google included) is rewritten by
the mobile network and does not locate the sender of the RST. Still to check on the server side (reverse
proxy logs, firewalls) for the Jetson's 5G address.

### 2026-09-25 — Cause of the 5G block and real streaming over 5G

Diagnosis carried out on the server side (packet captures on the reverse-proxy host):
- The server's domain has **both an A and an AAAA record**. The 5G APN `orange` is IPv6-only (IPv4 through 464XLAT): the Jetson therefore tries IPv6
  first.
- Over IPv6 from the Internet: **port 80 reaches the reverse proxy**, whereas **443 and 8443 never get there** (no
  packet captured on the host). 8443 is silently dropped, and for 443 a third party (Livebox) answers
  SYN-ACK then RST. Hence the Livebox IPv6 firewall, which only lets port 80 through to this host. The
  phone "that works" was using IPv4.
- **Over IPv4 through 5G: HTTPS OK** (200, TLS in 0.17 s).
- No IP filtering on the reverse proxy; the media does not go through it (see below).

Real streaming over 5G (signaling forced to IPv4 through `/etc/hosts` + temporary route, LAN paths
neutralized): ICE path = **UDP IPv6, Jetson 5G address → LiveKit server's global IPv6** (host
candidate). **RTT ~45 ms, 0 % loss, stable 1.2 Mbps AV1**, jitter 6–9 ms, ~9 MB each way in 60 s. IPv6
media therefore goes through the box. Only IPv6 TCP 443 signaling is blocked.

Test pitfall: as long as the Jetson is also on the LAN (Wi-Fi), ICE picks the direct LAN path, and the test
no longer goes over 5G (0 bytes on `wwan0`). The LAN routes to the SFU must be neutralized.

### 2026-09-28 — Transparent TCP proxy on Orange 5G

Livebox remote access disabled, and IPv6 rules targeting the reverse-proxy host as expected (checked by the
user). Fingerprint of the SYN-ACKs received on `wwan0`: **identical for every destination** (reverse
proxy :80 and :443, Cloudflare :443: `win 63940, mss 1390, wscale 6`), whereas the reverse-proxy host
actually answers with MSS 1440. Orange 5G (APN `orange`) therefore terminates **every** TCP connection with a transparent proxy
(split TCP), which spoofs both ends. That also explains the uniform hop limit of 62.

Corrected reading of the observations:
- :80: the proxy accepts on the Jetson side, then opens its own connection to the reverse proxy with the
  Jetson's address (seen by the host with hlim 54). OK.
- :443: the proxy accepts on the Jetson side, but its connection to the reverse proxy **never reaches the host** (no
  packet captured). It gives up after ~1.7 s and sends a RST to the Jetson.
- :8443: not intercepted by the proxy, and silently dropped upstream of the host.

Since the Livebox is the only element between the Internet and the reverse-proxy host, inbound IPv6 to 443 is
indeed lost at the box despite a correct rule (firmware bug or limitation?). To confirm from an external
IPv6 network without a proxy (online IPv6 port tester). Program side: the Orange proxy may hide/alter TCP
behavior (RTT, MSS): LiveKit signaling (TCP WebSocket) goes through it, whereas the media (UDP) is not
affected.

### 2026-09-28 — Step 2: recording (program, 2 cameras + 2 microphones)

Release build, both cameras 1080p30 MJPEG, C-Media microphone (S16LE mono) and camera microphone (S24LE
stereo after the refused 24-bit mono), no override.

| Test | Result |
|---|---|
| Detection at startup | ✅ 4 devices, names `cam-usb-camera`, `cam-hd-usb-camera`, `mic-usb-camera`, `mic-usb-audio-device` |
| Recording files | ✅ H.264 High 1080p30, 30.03 fps constant, 1st frame is a keyframe, `tmcd` present (e.g. 13:18:32:15); BWF with `TimeReference` |
| Sync | ✅ microphones start at ~.43/.64 s, cameras ~0.5–1 s later (consistent with their start-up) |
| Stop/start (SIGUSR2/SIGUSR1) | ✅ every file finalized, new files on restart (keyframe first, new timecode), capture never stopped |
| `devices.yml` change + SIGHUP (name + rotation 90°) | ✅ only the 2 affected devices restarted; rotated file 1080×1920 |
| Critical disk (threshold above free space) | ✅ capture runs, no file created |
| `kill -9` after 30 s | ✅ videos readable to the last second (870/900 frames), WAV complete (30.0 s) |
| CPU (whole process) | **26 % of one core** (8 cores), i.e. ~13 % per camera |
| Memory | 746 MB RSS, stable over 90 s (`gst-launch` alone: 409 MB for one camera — NVIDIA libraries and buffers) |

### 2026-09-28 — Step 3: telemetry (program, desktop session)

| Test | Result |
|---|---|
| UPS | ✅ `ups_*.csv` every 2 s: 12.492 V, 97.0 %, current/power signed as in the reference script |
| Modem | ✅ every 5 s: `connected`, `lte+5gnr`, `Orange F`, quality 81–84 %, LTE RSRP −89/−90 dBm, SINR 10.8–12.2 dB, cell `13B2A09` / TAC `6626`, bearer connected; NR sentinels (−32768) left empty |
| GPS | ✅ GNSS enabled in unmanaged mode, NMEA read on `/dev/ttyUSB1` (found through ModemManager); indoors: `fix=none`, one line per second |
| System | ✅ every 5 s: temperatures (CPU 52 °C, junction 54 °C), load, RAM, NVENC 179 MHz while encoding, 122 GB free, `recording=true`, 2 cameras, 2 mics, mode `15W` |
| UPS unreachable (wrong address) | ✅ one warning, then lines with empty fields, no skipped line |
| Recording stop/start | ✅ CSV files finalized and reopened together with the audio/video files |

### 2026-09-28 — Step 4: LiveKit streaming

Prototypes first (local `livekit-server --dev`, HD camera, 960×540 AV1):

| Feeding path | Result |
|---|---|
| Pre-encoded (`nvv4l2av1enc` → `capture_encoded_frame`) | Rate control OK (target 805 → 1200 kbps relayed to the encoder), but **no keyframe on demand**: 1 keyframe in 45 s for 58 requests, late subscribers never decode. NVIDIA's GStreamer encoders ignore force-key-unit events (AV1 and H.264, JetPack 7) → rejected |
| DMA-BUF (NVMM fd → SDK Jetson AV1 encoder) | ✅ keyframes on demand (late subscriber decodes at once), 0 loss, 15–17 % CPU for the whole prototype (capture included), vs 23 % for the raw path |

Program (2 cameras recorded + streamed, 2 microphones, telemetry):

| Test | Result |
|---|---|
| Stream branch freezing the capture after 4 frames | Found and fixed: held NVMM frames pin decoder surfaces → `num-extra-surfaces=10` (reproduced and confirmed in a Python pipeline without the SDK) |
| Local server, viewer 60 s | ✅ 2 AV1 tracks decoded by dav1d at 25–30 fps, 0 loss; 2 Opus tracks; `main_camera` attribute; 55 metadata updates in 55 s; recordings unaffected (one file per camera, 64 s, 30.02 fps) |
| `mute()` of a DMA-BUF track (load shedding) | ❌ SDK segfault → pause by unpublishing, plus a 30 s warm-up (it had triggered during the bandwidth estimate ramp-up). The automatic pause was then removed (decision of 2026-09-28) |
| Clean shutdown | ✅ 0.4 s |
| CPU, release build | **57 % of one core** (4 cores online in 15W) for everything; recording alone was 26 % |
| Memory | 933 MB RSS |
| Real server `wss://live.minarox.fr` over Wi-Fi | ✅ room `racecast` created by the car, 4 tracks, `Jetson MMAPI AV1 Encoder`, viewer 25–30 fps with 0 loss, metadata 30 updates in 30 s |
| Secrets in logs | ✅ secret never logged, API key masked (4-character prefix) |

### 2026-09-28 — Step 5: system setup

| Test | Result |
|---|---|
| Load in 15W, 2 cameras recorded + streamed (real server over Wi-Fi), 2 microphones, telemetry, 60 s of `tegrastats` | CPU 21 % on average over the 4 cores (max 32 %), frequency ≤ 1.19 GHz; VDD_IN **6.6 W** vs 5.0 W idle (+1.6 W); tj ≤ 55 °C; program RSS 928 MB, stable over 8 min → **15W kept** |
| 10W vs 15W, same load (2 × 60 s of `tegrastats` + devfreq clocks) | 10W: VDD_IN **6647 mW** vs 6624/6625 mW in 15W (within noise, σ ≈ 20–80 mW); idle 5088 vs 5000 mW (noise); EMC at 2133 MHz in both modes (the 10W cap: never above it in 15W either); NVENC ≤ 371 MHz, VIC ≤ 218 MHz (max ~800 MHz) → **no gain, back to 15W** |
| Journal flood | Found: NVIDIA's `NvVideoEncoder::setBitrate` (JetPack 7 MMAPI classes, compiled into the SDK) prints two pixel formats (`808539713` = `AV10`, `842091854` = `NV12`) on stdout at every bitrate update: ~12 lines/s on a real network (6090 lines in 8 min). Fixed: stdout → `/dev/null` at startup, console logs on stderr. After: stdout empty, 92 lines of stderr in 60 s |
| chrony SOCK sample format | ✅ user-mode chronyd 4.5 from the Ubuntu package fed with the same 40-byte layout: source `GPS` reachable, offset −0.25 s shown as `+250ms`, root distance ±250 ms (= `delay 0.5`); socket created with mode `0777 & ~umask` (hence the permission drop-in) |
| `deploy/` static checks | ✅ `bash -n`, `systemd-analyze verify` (service and drop-in line), polkit rule evaluated under Node: `Location` and `Device.Control` allowed to `jetson` only |
| Shutdown | An SDK `ERROR failed to negotiate the publisher … Called in wrong state: closed` was seen once during a clean shutdown (renegotiation racing the room close); harmless, not reproduced |
| `sudo deploy/install.sh` | ✅ no error; chrony 4.5 installed (`systemd-timesyncd` removed), sysctl at 100/100, socket `root:jetson 0660`, service enabled, 15W |
| Service start | ✅ `Type=notify` ready, watchdog 15 s pings, 2 cameras + 2 microphones recorded, 4 tracks on the real server over Wi-Fi (hardware AV1), telemetry CSV; 79 journal lines in 2 min, 0 warning/error, 0 vendor debug line |
| ModemManager without a session | ✅ GNSS read, extended signal values present; `pkcheck` on the service process: `Location` and `Device.Control` authorized. It also reports `Control` and `Firmware` authorized: granted by an older rule of the user, `/etc/polkit-1/rules.d/52-modemmanager-jetson.rules` (broader access to the modem, e.g. `mmcli` over SSH). An older `/etc/polkit-1/localauthority/50-local.d/50-modemmanager.pkla` (same two actions) is likely ignored (`polkitd-pkla` not installed) |
| `SIGHUP` (what `systemctl reload` sends) | ✅ `devices.yml re-read, no change` |
| Watchdog: process frozen with `SIGSTOP` | ✅ killed by systemd after 30 s (`SIGABRT`), restarted after 29 s in total, `NRestarts=1`; no core file (soft `LimitCORE` 0, apport ignores unpackaged binaries); the interrupted `.mov` files decode to the end (4140 and 4170 frames, 30.01 fps, up to the freeze) |
| GPS → chrony | ✅ 3D fix with the car parked (4–5 satellites, HDOP 1.6); source `GPS` reachable, not selected nor combined (`-`) while NTP is up (`*`, ±8 ms); GPS offset **+20 ms**, std dev 0.75 ms over 200 s → `offset 0.020` |
| Watchdog at 10 s (`SIGSTOP`, 2026-09-29) | ✅ timeout 6.9 s after the freeze (last ping ~3 s before it), new process at +9.6 s, video recording again at +10.4 s, streaming (hardware encoder) at +11.9 s; worst case ≈ 13.5 s. 15.5 h overnight before it: 0 late ping, 0 warning |
| Safe configuration application (service, `cam-usb-camera`, 2026-09-29) | ✅ `SIGHUP` without change → "no change"; rotation 180 → only that camera restarted, confirmed 5.2 s after its capture start, `last-good` updated; forced failure (day directory read-only, rotation 90/270, 3 runs) → failure detected 0.1–0.4 s after the capture start, camera back to 180, `devices.yml` restored, `devices.yml.rejected` written; second `SIGHUP` during a confirmation → refused; file edited while stopped → startup with the confirmed version, then transaction confirmed (source `startup`); original file restored and confirmed |
| Recording failure = camera restart (bug found by the test above) | Fixed: a recording branch that fails to start is removed at once, but its `filesink` error reached the bus afterwards and was taken for a capture error → the whole camera (and its stream) restarted in a loop while the disk was not writable. Errors are now classified by origin: element no longer in the pipeline → ignored (already handled), branch being removed → `warn`, only capture-chain elements stop the camera. After: 4 failed recording starts per camera in 20 s (read-only directory), capture and stream untouched, recording back on its own once writable. A capture stall was seen once right after a rollback (restored by the supervisor in 1.5 s), not reproduced in 3 more runs |
| SDK `ERROR failed to negotiate the publisher` at shutdown | Fixed: seen on 4 of 5 clean stops (the room task unpublished tracks of devices stopping at the same time, and the renegotiation raced the room close). The room task no longer unpublishes once shutting down; the manager handles cancellation before its channels close. After: 5 clean stops in a row, 0 warning/error |
| GPS fix logs (2026-09-29) | ✅ debounced: 10 s of stable state before "GPS fix acquired" / "GPS fix lost", 2D ↔ 3D not logged (unit tests: fix flapping 4 s on / 3 s off for 2 min → no line). Service, 3 min indoors: 2 lines, each 10 s after the real change in the CSV (was 1355 lines over the previous night) |
| Main camera bitrate priority (2026-09-29, real server over Wi-Fi, viewer measuring received bitrate per track every 10 s) | ✅ Uplink not capped: both cameras at ~1.2 Mbps (their maximum), priority irrelevant. Uplink capped at 1.5 Mbps (`tc tbf` on `wlP1p1s0`): **no main camera → 502–557 / 505–574 kbps** (equal split); **`cam-hd-usb-camera` main (priority 4.0, read back from libwebrtc) → 813–885 kbps vs 235–246 kbps** for the other (ratio ~3.5, minimums and overheads). Same total video (~1.08 Mbps), 29–31 fps and 0 loss on every track in both cases, audio 64 kbps each. Changing the main camera went through the confirmed configuration transaction |

