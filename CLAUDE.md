# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## What this is

RaceCast Emitter is a Go daemon that runs on an NVIDIA Jetson (Orin NX) mounted in a race car. It captures video (USB cameras) and audio (USB mics), records them to local MP4 files, and simultaneously streams a lower-bitrate encode over a cellular modem via SRT to a remote receiver. It also reports UPS (battery) telemetry and GPS/modem signal telemetry to the receiver over the same SRT link, and receives IDR keyframe requests back from it. There is no receiver code in this repo — only the emitter side.

The target hardware is fixed: Jetson Orin NX (aarch64) with an I2C UPS module, a Quectel cellular modem (RM520N-GL) exposing NMEA over serial and managed via ModemManager/D-Bus, and USB cameras/mics identified by udev UID.

## Build & run

Requires GStreamer 1.0 (with NVIDIA Jetson hardware plugins: `nvv4l2h264enc`, `nvv4l2av1enc`, `nvvidconv`, `nvv4l2decoder`), libsrt, and CGo — this only builds/runs meaningfully on the Jetson itself (or an aarch64 environment with the same libraries and `/dev/v4l`, `/dev/snd`, D-Bus/ModemManager access).

```bash
go build -o racecast-emitter .   # build
go vet ./...                     # static check (no test suite exists)
```

Run modes (mutually exclusive flags):

```bash
./racecast-emitter                 # normal operation: record + stream per devices.yaml
./racecast-emitter --record        # recording only, no SRT streaming
./racecast-emitter --stream        # streaming only, no local recording
./racecast-emitter --ups [interval]  # standalone: print UPS readings (I2C), Ctrl+C to stop
./racecast-emitter --modem           # standalone: print GPS fix + modem signal, Ctrl+C to stop
```

Configuration is via `.env` (loaded at startup, see `.env.example` for all `RC_*` variables — SRT host/port/passphrase/latency, modem NMEA port, UPS I2C address/bus, video/audio bitrates, intra-refresh period) and `devices.yaml` (declares cameras and microphones by udev UID, capture resolution/framerate/flip, and optional per-device `stream:` block). A device with no `stream:` block or `bitrate: 0` is recorded but never streamed. `devices.yaml` documents in comments how to discover UIDs (`udevadm info --query=property --name=/dev/videoX | grep ID_SERIAL`) and supported formats (`v4l2-ctl --list-formats-ext`).

There is no test suite, linter config, or CI in this repo.

## Architecture

### Process lifecycle (`main.go`)

On startup, one `pipeline.Slot` is allocated per enabled camera/microphone. A control goroutine calls `pipeline.Poll` immediately, then again (with retries at 300ms/500ms/1s/2s) whenever a udev "add" event fires — this handles devices that take a moment to become usable after being plugged in. A single bidirectional SRT `telemetry.Conn` (streamid `"telemetry"`) is shared for sending UPS/modem JSON payloads out and receiving IDR requests in. `modem.WatchConnectivity` pauses/resumes all stream pipelines (not record pipelines) when the modem loses/regains internet, so recording is unaffected by connectivity. Shutdown: SIGINT/SIGTERM cancels the root context, notifies the receiver of stream closes, sends EOS to record pipelines (so MP4 muxing finalizes cleanly) while cancelling source/stream pipelines immediately, then waits for all pipeline goroutines — a second signal forces `os.Exit`.

### GStreamer pipeline model (`internal/pipeline`)

Each device (camera or mic) is a `Slot` holding up to three independent, separately-lifecycled GStreamer pipelines, connected via GStreamer `inter*` elements (`intervideosink`/`intervideosrc`, `interaudiosink`/`interaudiosrc`) rather than being one monolithic pipeline:

- **source** — always running while the device is present. Captures raw frames/samples once and `tee`s them into two named inter-channels (`rc:v rec:<name>` / `rc:v str:<name>`, and the audio equivalents), one per consumer. Inter-sinks silently drop data when no consumer pipeline is attached, so record/stream can be started or stopped independently without disturbing capture.
- **record** — reads from the record inter-channel, encodes H.264 (video) or AAC (audio, muxed with a synthetic black H.264 track so ffmpeg-family tools always see a video stream) into a fragmented MP4 under `records/<date>/<time>_<name>_<video|audio>.mp4`. Stopped via EOS so `mp4mux` flushes.
- **stream** — reads from the stream inter-channel, encodes AV1 (video, `nvv4l2av1enc`) or Opus (audio) and sends via `srtsink` in caller mode to `RC_SRT_HOST:RC_SRT_PORT` (SRT `streamid` = `"<name>:camera"` or `"<name>:microphone"`, used by the receiver to route/label tracks). Paused/resumed (not torn down) on connectivity changes for fast recovery.

`pipeline.Poll` starts pipelines in two phases per call: phase 1 starts any missing source pipelines, phase 2 starts record/stream pipelines only for slots whose source is confirmed running — this ordering matters, don't collapse it. Within each phase, all new pipelines are started concurrently under one `StartEach` call, which briefly redirects fd 1/2 to `/dev/null` (`silenceMu`/`silence_begin`/`silence_end` in `gst.go`) to suppress noisy NVIDIA plugin stderr output during the async state change — this silencing is global and mutex-serialized, so don't add new code paths that write to stdout/stderr from a pipeline-starting goroutine without going through the same lock.

`GstPipeline` (`gst.go`) wraps a `gst_parse_launch`-built pipeline via cgo, using two distinct contexts: `controlCtx` (parent, checked only at startup) and an internal drain `ctx` (independent, so cancelling the parent context doesn't skip EOS-based MP4 finalization — only `Cancel()`/EOS/timeout ends `watchBus`). Runtime control of named elements happens via `gst_bin_get_by_name` + property/signal calls, not by rebuilding the pipeline: `SetBitrate`, `ForceIDR` (GLib signal `force-IDR`, not a property — Jetson encoders), `TrySetIntraRefresh`, `SetValve`, `GetSRTSinkStats`. Pipeline description strings that use these must name the relevant elements consistently (`avenc` for the video encoder, `srtsink` for the SRT sink) — see `build.go`.

Pipeline description strings are assembled as plain GStreamer launch syntax in `build.go` (`BuildVideoSourceStr`, `BuildVideoRecordStr`, `BuildVideoStreamStr`, and audio equivalents). Comments there explain non-obvious buffer-format choices (I420 vs NV12, when the VIC is invoked vs `NvBufSurfaceCopy`) — read them before changing capture/encode caps, since a mismatched format silently forces a slow CPU path on the Jetson.

### Local adaptive bitrate (`internal/pipeline/feedback.go`)

Each stream pipeline runs an independent ABR loop (`WatchLocalStats`) that polls `srtsink`'s cumulative `stats` property every 2s (RTT, bandwidth, packet loss — no round trip to the receiver needed) and adjusts the AV1 encoder bitrate via `SetBitrate`. Decrease is immediate on degradation (loss > 3% or RTT > 150ms, ×0.70); increase requires 3 consecutive stable samples (loss < 0.5% and RTT < 200ms) before stepping up ×1.10, capped at the device's configured max. Independently, `internal/modem.BitrateAdvisoryFromStats` derives a pre-emptive ceiling from cellular radio technology (5G/LTE unrestricted, HSPA+ 85%, UMTS 55%, 2G floor 20%) and signal quality, enforced immediately on downgrade. IDR is forced once per SRT (re)connection and on every stream resume after a connectivity outage, since the receiver rebuilds its decode pipeline on reconnect.

### Telemetry (`internal/telemetry`)

A single persistent SRT connection (cgo, libsrt) carries structured JSON envelopes both ways: `internal/ups` and `internal/modem` each have a `RunStream` goroutine that marshals a typed payload (`{"type":"ups",...}` / `{"type":"modem",...}`) and calls `Conn.Send`; `main.go` registers the receive callback that dispatches `{"type":"idr","camera":"..."}` requests to the matching camera's `Slot.ForceIDR`. `Conn` redials transparently on send/receive failure — callers don't need reconnect logic.

### Device discovery (`internal/devices`, `internal/udev`)

Cameras/mics are matched by udev UID (from `devices.yaml`) rather than `/dev/videoN` index, because Jetson enumeration order isn't stable across reboots or USB replug. `devices.FindVideo`/`FindALSA` prefer `/dev/v4l/by-id` or `/dev/snd/by-id` symlinks and fall back to scanning with `udevadm`. `internal/udev.Listen` opens a raw `NETLINK_KOBJECT_UEVENT` socket directly (no external library) to trigger re-polling on device hotplug.

### Modem (`internal/modem`)

Talks to ModemManager over D-Bus (`godbus`) for signal quality/technology and to enable `gps-unmanaged` mode, but reads raw NMEA sentences itself from the modem's serial GPS port (checksum-validated, grouped into epochs on each `GGA` sentence) rather than going through ModemManager's location API. Subscribes to ModemManager `InterfacesAdded`/`InterfacesRemoved` signals to transparently reattach after the modem's USB device re-enumerates (its D-Bus object path and `/dev/ttyUSBx` node can both change) — see `attachModem`/`watchModemManager`. `WatchConnectivity` is what drives the stream pause/resume in `main.go`; it gives up watching entirely (assumes no modem installed) only if the modem never responds at all in the first 10s.

### Logging (`internal/logger`)

Writes to both a daily-rotated file under `logs/<date>.log` and the console. Because GStreamer startup silencing (see above) redirects fd 1/2 to `/dev/null` temporarily, the logger dup()s the original stdout fd at `init()` time (`consoleOut`) so log output keeps appearing on the terminal even during that window.
