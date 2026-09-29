#!/usr/bin/env bash
# System setup of RaceCast-Emitter (docs/SPEC.md §9c): groups, chrony with the modem GPS as a fallback
# time source, polkit rule for ModemManager, page cache write-back, systemd service. Idempotent: run it
# again after changing a file of deploy/ or after `cargo build --release`.
#
#   sudo deploy/install.sh
set -euo pipefail

USER_NAME=jetson
POWER_MODE=15W # nvpmodel mode chosen after measuring the real load (SPEC §9c)
DEPLOY="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
BINARY="$(dirname "$DEPLOY")/target/release/racecast-emitter"

step() { printf '\n==> %s\n' "$*"; }

if [[ $EUID -ne 0 ]]; then
    echo "run as root: sudo $0" >&2
    exit 1
fi
if [[ ! -x $BINARY ]]; then
    echo "missing $BINARY: run 'cargo build --release' first (as $USER_NAME)" >&2
    exit 1
fi

step "Groups of $USER_NAME: cameras and GPU (video, render), microphones (audio), UPS (i2c), NMEA port (dialout)"
usermod -aG video,render,audio,i2c,dialout "$USER_NAME"

step "Time: chrony (NTP first, modem GPS as fallback) instead of systemd-timesyncd"
if ! dpkg-query -W -f='${Status}' chrony 2>/dev/null | grep -q 'install ok installed'; then
    apt-get install -y chrony # replaces systemd-timesyncd
fi
install -D -m 644 "$DEPLOY/chrony.conf" /etc/chrony/conf.d/racecast.conf
install -D -m 644 "$DEPLOY/chrony-override.conf" /etc/systemd/system/chrony.service.d/racecast.conf

step "polkit: ModemManager actions without a desktop session (GNSS, signal, modem reset)"
install -D -m 644 "$DEPLOY/polkit.rules" /etc/polkit-1/rules.d/50-racecast.rules

step "Page cache write-back within ~1 s"
install -D -m 644 "$DEPLOY/sysctl.conf" /etc/sysctl.d/90-racecast.conf
sysctl -q -p /etc/sysctl.d/90-racecast.conf

step "Services"
install -m 644 "$DEPLOY/racecast.service" /etc/systemd/system/racecast.service
systemctl daemon-reload
systemctl restart chrony
systemctl enable racecast
systemctl restart racecast

step "Power mode"
if nvpmodel -q 2>/dev/null | grep -q "NV Power Mode: $POWER_MODE\$"; then
    echo "$POWER_MODE (ok)"
else
    # Not changed here: a mode change may ask for a reboot interactively.
    echo "WARNING: expected $POWER_MODE, got: $(nvpmodel -q 2>/dev/null | head -1)"
    echo "         set it with: sudo nvpmodel -m <id of $POWER_MODE in /etc/nvpmodel.conf>"
fi

step "Done"
echo "status: systemctl status racecast chrony"
echo "logs:   journalctl -u racecast -f"
echo "time:   chronyc sources   (GPS appears as 'GPS', reachable once the modem has a fix)"
