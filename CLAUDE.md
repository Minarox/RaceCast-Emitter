# RaceCast-Emitter

**Rust** program embedded in a race car (Jetson Orin NX): captures USB cameras, USB microphones and
telemetry (modem GPS, modem state, UPS, system state), **records locally** (source of truth) and
**publishes simultaneously** to a **LiveKit** room (WebRTC).

Full specification, decisions and open points: **`docs/SPEC.md`** — read it before any design decision,
and update it whenever a decision is made.

## Language

**Everything in the repository is in English**: code, identifiers, comments, log and error messages,
documentation (`CLAUDE.md`, `docs/`), commit messages. The user writes instructions in French; answer
them in French, but write every file in English.

## Non-negotiable rules

- **Never crash.** Every source (camera, microphone, GPS, modem, UPS) runs in its own supervised task,
  restartable with backoff. No `panic!`/`unwrap()`/`expect()` on a path that touches hardware, the network
  or the disk: typed error + log + restart.
- **Local recording never stops because of LiveKit.** Every pipeline uses a `tee` with two independent
  branches; the stream branch is best-effort (leaky queue, isolated errors).
- **Hardware acceleration everywhere**, for performance and battery life: MJPEG decoding
  `nvv4l2decoder mjpeg=1`/`nvjpegdec`, conversion/scaling/rotation `nvvidconv`, encoding
  `nvv4l2h264enc`/`nvv4l2av1enc`. Never `x264enc`, `videoconvert`, `videoscale`, `videoflip`.
- **Video recording**: H.264 VBR (12 Mbps by default) → `qtmux fragment-duration=1000
  fragment-mode=first-moov-then-finalise` (.mov), **mandatory `tmcd` timecode track** (DaVinci Resolve
  sync). The timecode is set by a **Rust pad probe** (`VideoTimeCodeMeta` on the H.264 buffers at the
  `h264parse` output), **not** by `timecodestamper` (which forces an NVMM→RAM round trip, ×3.4 CPU).
  Timecode = capture timestamp (`base_time + PTS`, `realtime` pipeline clock), never the arrival time.
  Constant frame rate via `videorate skip-to-first=true` in NVMM (without `skip-to-first`, the start TC
  is wrong). Do not use `isofmp4mux`/`mp4mux` (no `tmcd`) or `fragment-mode=dash-or-mss` (corrupts the
  file when a `tmcd` track is present). Audio: one BWF file per microphone.
  **Rotation applied right at capture** (`nvvidconv flip-method` before the `tee`): recording and stream
  are both upright; changing the rotation starts a new file.
- **LiveKit streaming in AV1** (540p30 @ 1.2 Mbps by default, identical for every camera): a deliberate
  choice of quality under constrained bandwidth over viewer compatibility. Jetson hardware encoding
  through the SDK: `video_codec: AV1` + `simulcast: false` are mandatory, otherwise it silently falls
  back to software. Frames go to the SDK as **DMA-BUF** (NVMM surface fd, zero copy); the front-end
  contract is `docs/PROTOCOL.md`.
- **5G disconnections are a normal event** (`info`/`warn`), not an error.
- **Telemetry** (GPS, modem, UPS, system): local CSV + LiveKit publication **in the room metadata** (full
  state, one section per source, coalesced HTTPS ≤ 1 Hz; no data tracks). Modem and GNSS only through
  ModemManager (D-Bus); the program reads the NMEA port itself (no gpsd) and feeds the GNSS time to
  chrony (SOCK refclock `/run/chrony.racecast.sock`, fallback when NTP is unreachable). **No shutdown on a UPS
  threshold**: resilience to power cuts comes from the file formats and flushes (SPEC §5b).
- **Recording from startup, continuously**; a start/stop button will come later → every action goes
  through an internal command bus (future button, future LiveKit RPC control).

## Environment (observed on the machine)

- Ubuntu 24.04.5, L4T R39.2.1 (JetPack 7), GStreamer 1.24.2, systemd 255.
- Rust 1.98.1 via rustup (`source "$HOME/.cargo/env"` if needed) → **edition 2024**.
- GStreamer dev packages, clang/libclang, can-utils installed.
- **gst-plugins-rs 0.12.11** (branch for GStreamer 1.24) built from source (`~/src/gst-plugins-rs`, no apt
  package on noble): plugins `fmp4` (`isofmp4mux`, not used), `rswebrtc` (`whipclientsink`,
  `livekitwebrtcsink`), `rsrtp` (`rtpgccbwe`), `mp4` — installed stripped in
  `/usr/lib/aarch64-linux-gnu/gstreamer-1.0/`. To update: `cargo build --release -p <crate>` then
  `sudo install -m 644 -s`. `gstreamer1.0-nice` installed (required by webrtcbin).
- Storage: 256 GB NVMe shared with the system (~130 GB free).
- `ffprobe`/`ffmpeg` default to `h264_cuvid`, which does not exist on Jetson: check recordings with
  GStreamer (`qtdemux ! h264parse ! nvv4l2decoder`) or `gst-discoverer-1.0`.

## Stack

- `tokio` · `tracing` + `tracing-subscriber` (JSON, rotating file) · `serde` + `serde_yaml_ng` · `chrono`
  (local time, timecode).
- `gstreamer-rs` 0.25 (capture, NVENC, qtmux) · `udev` (hot-plug) · `rustix` (statvfs, poll, termios).
- `zbus` (ModemManager over the system D-Bus) · `i2cdev` (UPS INA219) · `socketcan` (phase 2).
- `livekit` **=0.9.1** + pinned sub-crates in `Cargo.toml` (0.9.2 is broken; `livekit-common` must stay
  0.1.3). Build settings (clang 23 as `CC`/`CXX`, `-lnvbufsurface`) are in `.cargo/config.toml`, so a
  plain `cargo build` works; the first build of `webrtc-sys` takes a while. **`livekit` and `libwebrtc` are
  patched copies in `vendor/`** (`[patch.crates-io]`, bitrate priority of a video sender; diff and upgrade
  procedure in `vendor/README.md`): never edit them beyond that patch.

## Configuration

- `.env` (ignored by git; keys documented in `.env.example`): LiveKit (URL, keys, `LIVEKIT_ROOM`,
  `LIVEKIT_IDENTITY`), `RECORDINGS_DIR`, logs, thresholds.
- **Cameras and microphones are hot-plug detected through udev** (not `GstDeviceMonitor`: PipeWire hides
  its providers): parameters chosen automatically under the global cap, recording + streaming started
  without prior declaration; automatic names `cam-<model>` / `mic-<model>`.
- **Defaults in `.env`** (cap, bitrates, microphones, telemetry periods). By default **every camera streams
  with the same settings**; no main camera in `.env`.
- `devices.yml` (`DEVICES_FILE`) = **empty at first**, only per-device overrides (identified by USB port
  `by-path` or by-id) added as devices get configured: `name`, rotation, bitrates, optional **main camera**
  role, set only through the LiveKit configuration (own stream settings, priority on degraded 5G,
  highlighted by the front end). `name` gives the file, log and LiveKit track names. Written by the
  program (admin page), re-read on `SIGHUP`; **validated, then applied as a transaction: kept only if the
  restarted devices deliver frames for 5 s, otherwise every changed device goes back and the file is
  restored** (rejected copy in `devices.yml.rejected`, SPEC §8); future control from an admin page through
  LiveKit RPC.

## Code conventions

- **Single crate, single binary**, split into modules by domain (`config`, `capture`, `recording`,
  `stream` (LiveKit — not named `livekit`, which would clash with the crate), `telemetry` (`gps`, `modem`,
  `ups`, `system`), `storage`, `logging`, `supervisor`…).
- Logs tagged by subsystem through the `task` field of the span set by the supervisor (`camera:<name>`,
  `mic:<name>`, `gps`, `livekit`, `modem`, `ups`, `system`, `storage`, `commands`…).
- **Adopted patterns**:
  - every long-running task goes through `Supervisor::spawn(name, Restart::Always|OnError, |token| async
    {…})`: restart with backoff (1 s → 60 s, reset after 60 s of stable run), panic logged and absorbed;
    the task stops when `token` is cancelled;
  - typed errors (`thiserror`) or `String`, logged by the supervisor; never `unwrap()`/`expect()` outside
    tests;
  - every action (signal, future button, future RPC) goes through `CommandBus` → `Dispatcher`;
  - configuration: `Settings::load` never returns an error (default + warning); an invalid `devices.yml`
    is never applied (last confirmed version kept, copy in `devices.yml.last-good`); a valid one goes
    through a confirmed transaction in `capture::manager` (`ChangeRequest`); device tasks report start,
    frames and every failure to their `capture::health::Health`;
  - config file writes: `storage::write_atomic`;
  - device tasks: `Supervisor::spawn_scoped` with a per-device token (cancelled on unplug/reconfiguration);
  - branches on a running `tee`: link with `PadLinkCheck::empty()` (nvv4l2decoder breaks caps queries),
    detach with an IDLE probe + EOS, wait for the EOS at the sink before removing the bin; every branch is
    a bin and bus errors are classified by origin (element no longer in the pipeline → ignored, branch →
    that branch only, capture chain → camera error): a failing branch never restarts the capture;
  - GStreamer state changes to `Null` in `spawn_blocking` (NVENC teardown is slow);
  - NVMM buffers held downstream pin their decoder surface: any new holding (queue, frames kept for an
    encoder) must fit in `nvv4l2decoder num-extra-surfaces` (10), otherwise the capture freezes;
  - LiveKit: devices register in `StreamHub` and run their stream branch only while their track is
    published; never `mute()` a DMA-BUF video track (crashes the SDK) — unpublish it to stop it; never
    unpublish once shutting down (renegotiation races the room close); the main camera's sender gets
    `bitrate_priority` 4.0 (SDK patch) — priority, never a pause, when the uplink is short;
  - NVIDIA's GStreamer encoders ignore force-key-unit events on JetPack 7: keyframes on demand only come
    from the SDK's own Jetson encoder;
  - stdout is redirected to `/dev/null` at startup (NVIDIA's MMAPI encoder prints debug lines there at every
    bitrate change): console logs go to stderr, never `println!` outside `--check-config`.
- **Tests**: unit tests **only on pure logic** (timecode, BWF, NMEA, UPS conversions, mode selection,
  config/validation/paths, protocol JSON, stream sizing, modem safeguards), added along the way → keep
  that logic separate from hardware access. A test that touches GStreamer types calls `gst::init()` itself
  (tests run in any order, in parallel). **No hardware mocks**: integration tests on the Jetson
  (`videotestsrc`/`audiotestsrc`, `ffprobe`, local `livekit-server --dev`, fault-injection scripts:
  unplugging, `kill -9`, full disk, network cut).
- **Branches and CI**: development on `dev`, pull requests to `main`. `.github/workflows/ci.yml` runs on
  every pull request to `main` (GitHub arm64 runner, no Jetson hardware): TruffleHog secret scan of the
  new commits (built-in detectors + LiveKit ones in `.github/trufflehog.yml`), `cargo fmt --check`,
  `cargo clippy --all-targets -- -D warnings` (any warning fails), `cargo test`. It overrides the Jetson
  settings of `.cargo/config.toml` (clang path, `libnvbufsurface`): keep the two in step.

## Commands

```bash
source "$HOME/.cargo/env"
cargo build --release          # binary: target/release/racecast-emitter
cargo test                     # unit tests (pure logic)
cargo clippy --all-targets -- -D warnings && cargo fmt --check   # same checks as the CI

./target/release/racecast-emitter --env-file .env --check-config   # validate .env and devices.yml
./target/release/racecast-emitter --env-file .env                  # manual run
```

Signals: `SIGTERM`/`SIGINT` clean shutdown, `SIGHUP` re-reads `devices.yml` (after a manual edit),
`SIGUSR1`/`SIGUSR2` start/stop local recording (until the button exists).

Full reinstall (new disk, fresh JetPack): `deploy/INSTALL.md`. System setup (SPEC §9c, run by the user,
sudo required): `sudo deploy/install.sh` — idempotent; installs
and restarts the service (`deploy/racecast.service`), chrony with the GPS refclock, the polkit rule for
ModemManager, the write-back sysctl, the groups. Re-run it after changing a file of `deploy/`. Never
touch `/etc/gai.conf` (IPv4 preference deliberately not applied). Logs: `journalctl -u racecast -f` or
`logs/racecast.<date>.log` (JSON); `sudo systemctl reload racecast` re-reads `devices.yml`; after
`cargo build --release`, `sudo systemctl restart racecast`. Stop the service before a manual run (devices
are exclusive).
