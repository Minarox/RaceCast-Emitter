# Patched LiveKit SDK crates

`libwebrtc` 0.3.48 and `livekit` 0.9.1 — the exact versions pinned in `Cargo.toml` — copied from crates.io
with a small RaceCast patch, and used instead of the registry versions through `[patch.crates-io]` in
`Cargo.toml`. The full diff is in `sdk.patch`; nothing else differs from the published crates.

Why (SPEC §6, "Prioritizing a stream"): give the main camera a larger share of a short uplink with
WebRTC's `bitrate_priority`, which the SDK did not let an application set.

| Crate | Change |
|---|---|
| `libwebrtc` | `RtpEncodingParameters` gets a `bitrate_priority` field (default 1.0), carried both ways between Rust and C++ instead of being reset to 1.0 (`webrtc-sys` already had it, so no C++ change and no rebuild of `webrtc-sys`) |
| `livekit` | `LocalVideoTrack::transceiver()` made public, so that the application reaches the track's `RtpSender` (`parameters()` / `set_parameters()`) |
| both | `[lints.rust] warnings = "allow"` in `Cargo.toml`: warnings of third-party code are not shown in our builds |

## License

Both crates are © LiveKit, Inc., under the Apache License 2.0, like this project. The published crates
do not ship the license text, so a copy is in `vendor/LICENSE`. As the license requires, every modified
source file carries a `Modified for RaceCast-Emitter` notice under its license header; the manifests carry
a `RaceCast:` comment next to the change. These notices are part of `sdk.patch`.

## Upgrading the SDK

1. Pick the new versions in `Cargo.toml` (keep the whole LiveKit set consistent, see `CLAUDE.md`).
2. Replace `vendor/libwebrtc` and `vendor/livekit` with the new published sources
   (`~/.cargo/registry/src/*/libwebrtc-<version>` and `livekit-<version>` after a `cargo fetch`, or the
   `.crate` archives from crates.io).
3. Re-apply the patch: `patch -p0 -d vendor < vendor/sdk.patch` (fix by hand if the context moved), then
   regenerate `sdk.patch` from the pristine copies. Keep the modification notices (see License).
4. Build, then check that the service logs `main camera bitrate priority set … priority=4.0` for a main
   camera.

If a future SDK exposes the bitrate priority itself, drop the patch: remove `[patch.crates-io]` and
`vendor/`, and use the SDK's API in `src/stream/room.rs`.
