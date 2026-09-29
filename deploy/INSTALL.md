# Reinstalling RaceCast-Emitter from scratch

Procedure for a fresh Jetson Orin NX or a new NVMe (e.g. the planned 500 GB one). If the old disk is
**cloned** onto the new one, none of this is needed: just check the service (step 7).

Reference machine (2026-09-28): Orin NX 8 GB (`p3767-0001`), JetPack 7 / L4T R39.2.1, Ubuntu 24.04,
GStreamer 1.24.2, Rust 1.98.1, user `jetson`.

## 0. Before wiping the old disk

Nothing below is recreated by this procedure; copy it somewhere safe first.

| What | Where | Why |
|---|---|---|
| Unpushed work | `~/Projects/RaceCast-Emitter` | the code is on GitHub (branch `rust` of `Minarox/RaceCast-Emitter`); push local commits first. The local `main` branch keeps the detailed pre-publication history, which is not on GitHub |
| `.env` | repository root | LiveKit URL, keys (secret), room, identity, local settings (not in git) |
| `devices.yml`, `devices.yml.last-good` | repository root (`DEVICES_FILE`) | per-device settings (names, rotation, main camera…) |
| Recordings and logs | `records/`, `logs/` (`RECORDINGS_DIR`, `LOG_DIR`) | never deleted by the program |
| WireGuard | `/etc/wireguard/wg0.conf` (private key: secret), `/usr/local/sbin/wg-watchdog.sh`, `wg-watchdog.service` / `.timer` | administration VPN (SPEC §9b), not part of this repository |
| NetworkManager modem connection | `nmcli connection show modem` (APN `orange`) | 5G data |
| Optional: your own polkit rule | `/etc/polkit-1/rules.d/52-modemmanager-jetson.rules` | broader `mmcli` access over SSH; the program does not need it |

## 1. Base system

1. Flash **JetPack 7** (L4T R39.2.1) with the user `jetson`.
2. Check the NVIDIA multimedia pieces the program and the SDK build need:
   ```bash
   dpkg -l nvidia-l4t-gstreamer nvidia-l4t-jetson-multimedia-api nvidia-l4t-multimedia-utils
   ls /usr/src/jetson_multimedia_api/samples/common/classes/NvVideoEncoder.cpp
   ```
   `nvidia-l4t-jetson-multimedia-api` provides the MMAPI sources compiled into the LiveKit SDK (Jetson AV1
   encoder); without them the SDK silently falls back to software encoding.
3. Power mode **15W** (SPEC §9c): `sudo nvpmodel -m 2` (mode ids are in `/etc/nvpmodel.conf`; `2` = 15W on
   `p3767-0001`).
4. Network: Wi-Fi, then the 5G connection (`nmcli connection add type gsm ifname '*' con-name modem apn
   orange`, or restore the saved one), then WireGuard (restore `wg0.conf`, `sudo systemctl enable --now
   wg-quick@wg0`, restore and enable `wg-watchdog.timer`). WireGuard is only for SSH administration; the
   program does not need it (SPEC §9b).

## 2. Packages

```bash
sudo apt install build-essential pkg-config git curl xz-utils \
  libgstreamer1.0-dev libgstreamer-plugins-base1.0-dev libudev-dev libglib2.0-dev \
  libx11-dev libdrm-dev libgbm-dev \
  gstreamer1.0-tools gstreamer1.0-plugins-base gstreamer1.0-plugins-good gstreamer1.0-plugins-bad \
  gstreamer1.0-alsa modemmanager i2c-tools
```

| Package(s) | Needed for |
|---|---|
| `libgstreamer1.0-dev`, `libgstreamer-plugins-base1.0-dev` | building `gstreamer-rs` (core, app, video) |
| `libudev-dev` | building the `udev` crate (hot-plug) |
| `libglib2.0-dev`, `libx11-dev`, `libdrm-dev`, `libgbm-dev` | building `webrtc-sys` (headers of its lazy-loaded desktop-capture stubs) |
| `gstreamer1.0-plugins-base` | `appsink`, `videorate` |
| `gstreamer1.0-plugins-good` | `v4l2src`, `qtmux` |
| `gstreamer1.0-plugins-bad` | `h264parse` |
| `gstreamer1.0-alsa` | `alsasrc` |
| `nvidia-l4t-gstreamer` (JetPack) | `nvv4l2decoder`, `nvv4l2h264enc`, `nvvidconv` |
| `modemmanager` | modem state and GNSS (D-Bus) |
| `i2c-tools` | optional: `i2cdetect -y 7` to check the UPS (INA219 at `0x41`) |

`chrony` is installed later by `deploy/install.sh`. Not needed: gpsd, gst-plugins-rs (only used for early
experiments), `livekit-server` (only for local tests, see step 8).

## 3. Rust and clang 23

```bash
curl --proto '=https' --tlsv1.2 -sSf https://sh.rustup.rs | sh -s -- -y   # stable, ≥ 1.88 (edition 2024, `time` ≥ 0.3.47)
source "$HOME/.cargo/env"

mkdir -p ~/src && cd ~/src
curl -LO https://github.com/llvm/llvm-project/releases/download/llvmorg-23.1.2/LLVM-23.1.2-Linux-ARM64.tar.xz
tar xf LLVM-23.1.2-Linux-ARM64.tar.xz        # → ~/src/LLVM-23.1.2-Linux-ARM64
```

The prebuilt libwebrtc of the LiveKit SDK ships a hermetic libc++ that needs **clang ≥ 21** (Ubuntu 24.04
has 18). `.cargo/config.toml` points `CC`/`CXX` at this exact path and adds `-lnvbufsurface`; if you use
another LLVM version or location, update that file.

## 4. Repository and configuration

```bash
mkdir -p ~/Projects && cd ~/Projects
git clone -b rust https://github.com/Minarox/RaceCast-Emitter.git
cd RaceCast-Emitter
cp .env.example .env    # or restore the saved .env; fill LIVEKIT_URL / LIVEKIT_API_KEY / LIVEKIT_API_SECRET
```

Restore `devices.yml` (and `devices.yml.last-good`) if you saved them; otherwise the program creates an empty
one and every device runs with the `.env` defaults. `records/` and `logs/` are created by the program.

## 5. Build

```bash
source "$HOME/.cargo/env"
cargo build --release     # first build: downloads the prebuilt libwebrtc (Internet), compiles webrtc-sys (long)
cargo test
./target/release/racecast-emitter --env-file .env --check-config
```

`Cargo.lock` is committed and the LiveKit crates are pinned in `Cargo.toml` (`livekit =0.9.1`,
`livekit-common =0.1.3`…): do not run `cargo update` on them (0.9.2 and `livekit-common` 0.1.4 break the
build, see `CLAUDE.md`). `livekit` and `libwebrtc` are built from the patched copies in `vendor/` (main
camera bitrate priority, `vendor/README.md`): nothing to do, they are part of the repository.

Optional manual run to check the devices before installing the service (stop it with Ctrl-C):

```bash
./target/release/racecast-emitter --env-file .env
```

## 6. System setup

```bash
sudo deploy/install.sh
```

Idempotent (SPEC §9c): groups (`video`, `render`, `audio`, `i2c`, `dialout`), chrony with the GPS refclock
(replaces `systemd-timesyncd`), polkit rule for ModemManager, write-back sysctl, then installs, enables and
starts `racecast.service`. It warns if the power mode is not 15W. **Never** add the IPv4 preference to
`/etc/gai.conf` (deliberately not applied, SPEC §9c).

## 7. Checks

```bash
systemctl status racecast chrony
journalctl -u racecast -f             # cameras/microphones detected, "track published", "hardware encoder in use"
chronyc sources                       # NTP servers, plus GPS once the modem has a fix
ls -l /run/chrony.racecast.sock       # srw-rw---- root jetson
ls records/$(date +%F)/               # .mov, .wav and .csv files growing
```

Expected in the journal: no `WARN`/`ERROR` at startup, `hardware encoder in use … Jetson MMAPI AV1 Encoder`
for every camera (an `ERROR … software video encoder in use` means the MMAPI sources or `simulcast: false`
are missing). On the front end: one AV1 track per camera, one Opus track per microphone, room metadata
updated every second (`docs/PROTOCOL.md`).

If the GNSS was never set up on this modem, the first fix can take several minutes outdoors; the chrony
`offset` (NMEA latency, `deploy/chrony.conf`) was measured on this modem and only needs re-measuring with a
different one (SPEC §11).

## 8. Optional: local test server

For integration tests without the real server (SPEC §12): download the `livekit-server` release binary for
`linux_arm64` from <https://github.com/livekit/livekit/releases> into `~/src/livekit-server/`, run
`livekit-server --dev`, and point a copy of `.env` at `ws://127.0.0.1:7880` with the `devkey` / `secret`
development keys. Stop the service first (`sudo systemctl stop racecast`): the devices are exclusive.
