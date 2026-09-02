package modem

// kernel_log.go folds modem_watch.sh's kernel-log monitoring into the app
// itself: a `journalctl -kf` tail, filtered to USB power/enumeration fault
// signatures (undervoltage, over-current, disconnect, reset) and to the
// modem's own USB bus path, so a genuine hardware issue on the modem's link
// shows up in the app's own daily-rotated log — reachable while debugging a
// live connectivity problem, without a separate script running alongside
// the app.

import (
	"bufio"
	"context"
	"os/exec"
	"regexp"
	"strings"
	"sync"
	"time"

	dbus "github.com/godbus/dbus/v5"

	"racecast-emitter/internal/logger"
)

// faultPattern matches the same class of kernel log lines modem_watch.sh
// looked for: USB power/enumeration faults that can explain a modem dropout
// that isn't visible at the ModemManager level at all.
var faultPattern = regexp.MustCompile(`(?i)xhci|usb.*reset|disconnect|over-?current|cannot enable|not enough power|under-?voltage|brownout`)

// WatchKernelLog tails the kernel log and logs lines matching faultPattern,
// or mentioning the modem's own USB bus path (from the Device D-Bus
// property, e.g. "1-2.3") once known. Runs for the life of ctx, restarting
// the journalctl pipe if it exits unexpectedly. Best-effort: if journalctl
// itself isn't usable (no permission, no journald) this logs a warning once
// and gives up — it's a diagnostic aid, not core functionality.
func WatchKernelLog(ctx context.Context) {
	var warnOnce sync.Once
	for {
		if ctx.Err() != nil {
			return
		}
		if err := tailKernelLogOnce(ctx); err != nil {
			warnOnce.Do(func() {
				logger.Warn("[modem] Kernel log watcher unavailable: %v", err)
			})
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(5 * time.Second):
		}
	}
}

// tailKernelLogOnce runs one journalctl process until it exits or ctx is
// cancelled. "-n 0" means no backlog is replayed on (re)start — only new
// lines are seen, so a restart never re-logs old history.
func tailKernelLogOnce(ctx context.Context) error {
	busPath := modemBusPath() // best-effort, "" if not currently known

	cmd := exec.CommandContext(ctx, "journalctl", "-kf", "-n", "0", "-o", "cat")
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	defer cmd.Wait()

	scanner := bufio.NewScanner(stdout)
	for scanner.Scan() {
		line := scanner.Text()
		if faultPattern.MatchString(line) || (busPath != "" && strings.Contains(line, busPath)) {
			logger.Warn("[modem] kernel: %s", line)
		}
	}
	return scanner.Err()
}

// modemBusPath returns the trailing USB bus-path component of the modem's
// sysfs Device property (e.g. "1-2.3" from
// "/sys/devices/platform/bus@0/3610000.usb/usb1/1-2/1-2.3"), or "" if the
// modem isn't attached or the property can't be read.
func modemBusPath() string {
	conn, path, ok := currentModem()
	if !ok {
		return ""
	}
	obj := conn.Object("org.freedesktop.ModemManager1", path)
	var v dbus.Variant
	if err := obj.Call("org.freedesktop.DBus.Properties.Get", 0,
		"org.freedesktop.ModemManager1.Modem", "Device").Store(&v); err != nil {
		return ""
	}
	device, _ := v.Value().(string)
	if idx := strings.LastIndex(device, "/"); idx >= 0 {
		return device[idx+1:]
	}
	return device
}
