# RaceCast-Emitter

This program runs on board a race car. It captures the car's USB cameras, USB microphones and telemetry
(GPS, 5G modem, battery, system state).

- It **records everything locally**. The recordings are the source of truth, ready for editing in DaVinci
  Resolve.
- At the same time, it **streams live** to a [LiveKit](https://livekit.io) room over the car's 5G link.

It runs unattended from power-on to power-off. Connection drops, unplugged devices and power cuts are
normal events, not failures.

> [!WARNING]
> **This project depends heavily on its hardware.** It is written for the machine below and does not run on
> a regular PC as is:
>
> - **Build**: it needs NVIDIA's Jetson libraries (`libnvbufsurface`, the Jetson Multimedia API sources).
> - **Video**: it relies on the Jetson's hardware blocks through NVIDIA's GStreamer elements (`nvv4l2decoder`,
>   `nvvidconv`, `nvv4l2h264enc`) and the LiveKit SDK's Jetson AV1 encoder. There is no software fallback, by
>   design.
> - **Telemetry**: it talks to a specific 5G modem through ModemManager and to a specific UPS board over
>   I2C.
>
> Missing telemetry hardware is tolerated (logged, retried, CSV fields left empty). Missing video hardware
> is not.

## Hardware

| Part | Model | Used for |
|---|---|---|
| Computer | **NVIDIA Jetson Orin NX 8 GB** (`p3767-0001`) on the Engineering Reference Developer Kit, **JetPack 7** (L4T R39.2.1, Ubuntu 24.04), 15 W power mode | everything; its hardware blocks do all the video work (below) |
| 5G modem | **Quectel RM520N-GL** on a 5G HAT, managed by ModemManager, with a GNSS antenna | streaming uplink, network telemetry, **GPS** (built-in GNSS, NMEA port) |
| Power | **[Waveshare UPS Power Module C](https://www.waveshare.com/UPS-Power-Module-C.htm)**: 3S battery, INA219 sensor on I2C bus 7, address `0x41` | battery voltage, current, power and charge telemetry |
| Cameras | USB UVC cameras delivering **MJPEG** (today a *Generic USB Camera* 1080p30 with a built-in microphone and an *HD USB Camera* 1080p30 / 720p60; a 3rd, 1080p60 camera is planned) | video |
| Microphones | USB audio (UAC) microphones: camera built-in or standalone (today a *C-Media USB Audio Device*) | audio |
| Storage | NVMe SSD (256 GB, shared with the system) | recordings and logs |

Jetson hardware blocks in use (the CPU never touches a video frame):

| Block | Job |
|---|---|
| Hardware decoder (`nvv4l2decoder mjpeg=1`) | decodes the cameras' MJPEG |
| VIC (`nvvidconv`) | rotation, scaling, format conversion |
| NVENC | H.264 for recording (`nvv4l2h264enc`) and AV1 for streaming (the LiveKit SDK's Jetson encoder, fed with DMA-BUF surfaces, zero copy) |

## Features

### Plug-and-play capture

- Cameras and microphones are **detected through udev, including hot-plug**: plug one in and it records
  and streams, unplug it and its files are finalized and its tracks unpublished. Nothing is declared in
  advance.
- **Video mode chosen automatically**: the largest MJPEG resolution under a configurable cap (1080p60 by
  default), then the highest frame rate at that resolution. If the USB bandwidth runs out, it steps down to
  a lower mode.
- **Microphone format chosen automatically**: the native format is kept, with no resampling.
- **Automatic names** (`cam-hd-usb-camera`, `mic-usb-audio-device`) are used for files, logs and LiveKit
  tracks. They can be overridden per device.

### Local recording (source of truth)

- **Starts at boot and runs continuously.** A new file starts at each device connection. Nothing is ever
  deleted.
- **Video**: H.264 VBR (12 Mbps by default) in a fragmented QuickTime `.mov` with a **`tmcd` timecode
  track**, for multi-camera sync in DaVinci Resolve.
  - The timecode is the capture time of each frame, not its arrival time.
  - The frame rate is held constant.
  - Rotation is applied at capture.
- **Audio**: one **Broadcast WAV** file per microphone. It carries its time reference (`bext` and `iXML`)
  and switches to RF64 beyond 4 GB.
- **Telemetry**: one CSV file per source, with UTC timestamps on the same clock as the timecodes.
- **Power-cut resilient**: at most about 1 s of video is lost (1 s fragments), 1 s of audio (headers
  rewritten every second), and a few seconds of CSV.
- **Disk guard**: below a free-space threshold, the files are finalized cleanly and recording stops, while
  streaming continues.

### Live streaming to LiveKit

- **Video**: one **AV1** track per camera (540p30 at 1.2 Mbps by default), hardware encoded.
- **Audio**: one **Opus** track per microphone (64 kbps).
- **Telemetry** goes in the **room metadata**: the full state with one section per source, updated at most
  once per second.
- **Main camera** (optional, per device): it can have its own streaming settings. When the uplink is short,
  it keeps most of the bandwidth: WebRTC `bitrate_priority` through a small patch of the SDK, in `vendor/`.
  The other cameras slow down but never stop.
- **Recording never depends on streaming.**
  - The two run in independent branches, and the streaming branch is best-effort.
  - A 5G drop is logged as an ordinary event. The program reconnects with backoff, and the recording
    carries on.
- The contract with the viewer (front end) is in [`docs/PROTOCOL.md`](docs/PROTOCOL.md).

### Telemetry

| Source | What | Default period |
|---|---|---|
| `gps` | fix, position, altitude, speed, course, satellites, HDOP, GNSS time. The program reads the modem's NMEA port itself (no gpsd) | 1 s |
| `modem` | state, access technology, operator, LTE and 5G NR signal (RSRP, RSRQ, SINR), cell | 5 s |
| `ups` | battery voltage, current, power, charge percentage | 2 s |
| `system` | CPU/GPU temperatures and load, RAM, NVENC clock, free disk, recording/streaming state | 5 s |

### Time

- chrony uses NTP when the network is up.
- The program feeds the **GNSS time to chrony** (SOCK refclock) as a fallback when no NTP server is
  reachable.
- The RTC covers the time at boot.
- Video timecodes, audio time references, CSV timestamps and logs all come from this one clock.

### Resilience

- **The program never crashes.**
  - Every source (each camera, each microphone, GPS, modem, UPS) runs in its own supervised task.
  - A failing task restarts with backoff (1 s to 60 s) without affecting the others.
  - Panics are logged and absorbed.
- **systemd service** (`Type=notify`, restarted forever): a 10 s watchdog restarts a hung process, back up
  in about 13 s.
- The **5G modem** is monitored through ModemManager. A modem that has really failed is restarted, at most
  once every 10 minutes.
- **Logs** are structured JSON, rotated daily and kept, tagged per subsystem (`camera:<name>`, `gps`,
  `livekit`…).

### Configuration

- **`.env`** holds the defaults that every device uses: capture cap, bitrates, streaming settings,
  telemetry periods, LiveKit credentials. An invalid value falls back to a built-in default with a warning.
- **`devices.yml`** starts empty and holds only per-device overrides: name, rotation, bitrates, streaming
  on/off, main camera. A device is identified by its USB port or its by-id.
- **Changes are applied safely.**
  - A new `devices.yml` is validated first.
  - Only the affected devices restart, and the change is kept only if they deliver frames for 5 s.
  - Otherwise every change is rolled back and the file restored (the rejected copy is kept in
    `devices.yml.rejected`).

### Control

Every action goes through an internal command bus. Today the commands come from Unix signals:

| Signal | Action |
|---|---|
| `SIGTERM` / `SIGINT` | clean shutdown |
| `SIGHUP` | re-read `devices.yml` |
| `SIGUSR1` / `SIGUSR2` | start / stop local recording |

A physical button in the car and remote control through LiveKit RPC will use the same bus.

## Getting started

Full installation on a fresh Jetson (packages, Rust, clang 23 for the LiveKit SDK, system setup):
**[`deploy/INSTALL.md`](deploy/INSTALL.md)**.

On a machine that is already set up:

```bash
source "$HOME/.cargo/env"
cargo build --release
cargo test
./target/release/racecast-emitter --env-file .env --check-config   # validate .env and devices.yml
sudo deploy/install.sh                                              # service, chrony, polkit, sysctl, groups
journalctl -u racecast -f
```

Copy `.env.example` to `.env` and fill in the LiveKit URL and keys first.

The build settings are in `.cargo/config.toml` (clang 23, `-lnvbufsurface`), so a plain `cargo build` works.
Do not run `cargo update` on the LiveKit crates: they are pinned on purpose, and `livekit` / `libwebrtc` are
patched copies (see [`vendor/README.md`](vendor/README.md)).

## Repository layout

| Path | Contents |
|---|---|
| `src/capture/` | udev detection, mode selection, naming, device manager, configuration transactions |
| `src/recording/` | video pipelines (`.mov` + timecode), BWF writer |
| `src/stream/` | LiveKit room, video/audio tracks, room metadata |
| `src/telemetry/` | GPS/NMEA, chrony feed, modem (ModemManager D-Bus), UPS (INA219), system, CSV |
| `src/config/` | `.env` settings, `devices.yml`, validation, paths |
| `src/` (top level) | supervisor, command bus, logging, storage, systemd integration |
| `deploy/` | systemd service, chrony, polkit, sysctl, `install.sh`, `INSTALL.md` |
| `vendor/` | patched LiveKit SDK crates and the patch itself |
| `docs/` | specification and front-end protocol |

## Documentation

- [`docs/SPEC.md`](docs/SPEC.md): full specification, design decisions, open points, validation log.
- [`docs/PROTOCOL.md`](docs/PROTOCOL.md): what the front end receives (tracks, attributes, room metadata).
- [`deploy/INSTALL.md`](deploy/INSTALL.md): reinstalling from scratch.
- [`CLAUDE.md`](CLAUDE.md): non-negotiable rules and code conventions.

## Roadmap

- Recording start/stop button in the car.
- Admin page (device configuration through LiveKit RPC).
- CAN bus telemetry (phase 2).
- Pending field tests: 5G on track, GPS while driving, physical hot-plug, the 3rd camera at full load.

## License

Copyright 2026 Mathis Serrieres Maniecki.

Licensed under the [Apache License, Version 2.0](LICENSE).

The patched LiveKit SDK crates in `vendor/` are © LiveKit, Inc., under the same license. Their changes are
listed in [`vendor/README.md`](vendor/README.md).
