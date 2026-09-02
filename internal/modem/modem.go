package modem

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	dbus "github.com/godbus/dbus/v5"

	"racecast-emitter/internal/logger"
)

// defaultNMEAPort is the GNSS serial port (Quectel RM520N-GL: /dev/ttyUSB1).
// Configurable via RC_MODEM_NMEA_PORT.
const defaultNMEAPort = "/dev/ttyUSB1"

var (
	// openMu serializes Open() so concurrent callers (RunStream, WatchConnectivity,
	// WatchHealth all call ensureOpen() on their own ticker) never race to dial
	// D-Bus and attach to the modem at the same time.
	openMu sync.Mutex

	mu        sync.Mutex
	modemPath dbus.ObjectPath // D-Bus object path of the detected modem

	// NMEA cache updated by the background serial reader goroutine.
	// Contains all frames from the last complete GPS epoch.
	nmeaMu         sync.RWMutex
	nmeaEpoch      []string
	nmeaLastUpdate time.Time // time of the last published epoch

	// Handle to the current NMEA serial port (set by nmeaReaderLoop).
	nmeaFile *os.File
	// nmeaStop is closed to terminate the current nmeaReaderLoop (on Close,
	// or when attachModem swaps in a replacement modem object).
	nmeaStop chan struct{}

	dbusConn *dbus.Conn // D-Bus system bus connection

	// mmSignalStop is closed by Close() to terminate watchModemManager.
	mmSignalStop chan struct{}
)

// mmLocationSourceGpsUnmanaged = MM_MODEM_LOCATION_SOURCE_GPS_UNMANAGED (bit 4).
const mmLocationSourceGpsUnmanaged = uint32(1 << 4)

// mmPortTypeGPS = MM_MODEM_PORT_TYPE_GPS (ModemManager port-type enum).
const mmPortTypeGPS = uint32(5)

// Open initializes the modem and starts the NMEA serial reader.
// Enables gps-unmanaged mode (AT+QGPS=1); the GNSS sends frames autonomously.
// RC_MODEM_NMEA_PORT overrides the default port. Idempotent.
//
// Also subscribes to ModemManager's InterfacesAdded/InterfacesRemoved signals
// so that if the modem's USB device is later removed and re-enumerated (a new
// ModemManager object path — this is what makes `mmcli -L`'s index climb over
// time), the package transparently reattaches to the new object instead of
// forever holding a D-Bus path to a modem that no longer exists.
func Open() error {
	openMu.Lock()
	defer openMu.Unlock()

	mu.Lock()
	if modemPath != "" {
		mu.Unlock()
		return nil
	}
	mu.Unlock()

	conn, err := dbus.SystemBusPrivate()
	if err != nil {
		return fmt.Errorf("D-Bus system bus: %w", err)
	}
	if err := conn.Auth(nil); err != nil {
		conn.Close()
		return fmt.Errorf("D-Bus auth: %w", err)
	}
	if err := conn.Hello(); err != nil {
		conn.Close()
		return fmt.Errorf("D-Bus Hello: %w", err)
	}

	// Locate the first modem via ObjectManager.
	var managed map[dbus.ObjectPath]map[string]map[string]dbus.Variant
	mmObj := conn.Object("org.freedesktop.ModemManager1", "/org/freedesktop/ModemManager1")
	if err := mmObj.Call("org.freedesktop.DBus.ObjectManager.GetManagedObjects", 0).Store(&managed); err != nil {
		conn.Close()
		return fmt.Errorf("ModemManager not found: %w", err)
	}
	var path dbus.ObjectPath
	for p, ifaces := range managed {
		if _, ok := ifaces["org.freedesktop.ModemManager1.Modem"]; ok {
			path = p
			break
		}
	}
	if path == "" {
		conn.Close()
		return fmt.Errorf("no modem detected")
	}

	if err := attachModem(conn, path); err != nil {
		conn.Close()
		return err
	}

	mu.Lock()
	dbusConn = conn
	mu.Unlock()

	if err := conn.AddMatchSignal(
		dbus.WithMatchObjectPath("/org/freedesktop/ModemManager1"),
		dbus.WithMatchInterface("org.freedesktop.DBus.ObjectManager"),
	); err != nil {
		logger.Warn("[modem] Could not subscribe to ModemManager signals — won't auto-recover from a USB re-enumeration: %v", err)
		return nil
	}
	// StateChanged: no object-path filter, since the modem's D-Bus path
	// changes across reattaches (see handleMMSignal's InterfacesAdded case) —
	// matched broadly and logged on every transition. This is the in-app
	// replacement for tailing `journalctl -u ModemManager` externally: a
	// structured D-Bus signal instead of parsing log text, so it doesn't
	// depend on ModemManager's log wording.
	if err := conn.AddMatchSignal(
		dbus.WithMatchInterface("org.freedesktop.ModemManager1.Modem"),
		dbus.WithMatchMember("StateChanged"),
	); err != nil {
		logger.Warn("[modem] Could not subscribe to modem state-change signals: %v", err)
	}
	// 3GPP registration state (e.g. "searching" <-> "home"/"roaming") — the
	// arg0 filter matters here: without it we'd also get every Bearer
	// PropertiesChanged (connection stats update every few seconds while
	// connected), which would flood the 8-slot sigCh and could crowd out
	// the signals above.
	if err := conn.AddMatchSignal(
		dbus.WithMatchInterface("org.freedesktop.DBus.Properties"),
		dbus.WithMatchMember("PropertiesChanged"),
		dbus.WithMatchOption("arg0", "org.freedesktop.ModemManager1.Modem.Modem3gpp"),
	); err != nil {
		logger.Warn("[modem] Could not subscribe to 3GPP registration-state signals: %v", err)
	}
	sigCh := make(chan *dbus.Signal, 8)
	conn.Signal(sigCh)
	stop := make(chan struct{})
	mu.Lock()
	mmSignalStop = stop
	mu.Unlock()
	go watchModemManager(conn, sigCh, stop)

	return nil
}

// ensureOpen makes sure the package is attached to a modem, attempting Open()
// if not. Cheap to call from a loop on every tick when already attached (a
// single mutex-guarded read). Open() is idempotent and serializes concurrent
// attempts via openMu, so independent pollers (WatchConnectivity, WatchHealth,
// RunStream) never need to coordinate among themselves or give up permanently
// — they just keep calling this and pick up as soon as the modem appears.
func ensureOpen() bool {
	mu.Lock()
	ready := modemPath != ""
	mu.Unlock()
	if ready {
		return true
	}
	return Open() == nil
}

// currentModem returns the D-Bus connection and object path of the currently
// attached modem, or ok=false if none is attached.
func currentModem() (conn *dbus.Conn, path dbus.ObjectPath, ok bool) {
	mu.Lock()
	conn, path = dbusConn, modemPath
	mu.Unlock()
	return conn, path, conn != nil && path != ""
}

// attachModem enables gps-unmanaged on the given modem object, resolves and
// opens its NMEA port, and (re)starts the serial reader loop against it.
// Used both by Open() (initial attach) and by watchModemManager (reattach
// after the modem object was replaced).
func attachModem(conn *dbus.Conn, path dbus.ObjectPath) error {
	// Re-apply the configured mode/band profile on every (re)attach — see
	// radio_profile.go. Backgrounded so a slow mmcli call never delays GPS
	// startup below; independent of GPS setup succeeding or not.
	go applyRadioProfile(context.Background())

	// Enable gps-unmanaged: ModemManager sends AT+QGPS=1 but does not capture frames;
	// we read them from the serial port. signalLocation=false.
	modemObj := conn.Object("org.freedesktop.ModemManager1", path)
	if err := modemObj.Call("org.freedesktop.ModemManager1.Modem.Location.Setup", 0,
		mmLocationSourceGpsUnmanaged, false).Err; err != nil {
		return fmt.Errorf("Location.Setup (gps-unmanaged): %w", err)
	}

	portPath := resolveNMEAPort(conn, path)
	f, err := os.Open(portPath)
	if err != nil {
		return fmt.Errorf("NMEA port %s: %w", portPath, err)
	}

	stopReader() // stop any reader left over from a previous modem object

	mu.Lock()
	modemPath = path
	mu.Unlock()

	stopCh := make(chan struct{})
	mu.Lock()
	nmeaStop = stopCh
	mu.Unlock()
	go nmeaReaderLoop(portPath, f, stopCh)

	logger.Info("[modem] GPS started (gps-unmanaged, port: %s)", portPath)
	return nil
}

// resolveNMEAPort returns the device path of the modem's GPS/NMEA port.
// RC_MODEM_NMEA_PORT always overrides. Otherwise queries the modem's "Ports"
// property (type (su): port name, MM port-type enum) for the GPS port — this
// is what lets attachModem find the right device even when the kernel hands
// out fresh ttyUSB numbers after a USB re-enumeration. Falls back to
// defaultNMEAPort if the property can't be read or has no GPS entry.
func resolveNMEAPort(conn *dbus.Conn, path dbus.ObjectPath) string {
	if p := os.Getenv("RC_MODEM_NMEA_PORT"); p != "" {
		return p
	}

	obj := conn.Object("org.freedesktop.ModemManager1", path)
	var portsVariant dbus.Variant
	if err := obj.Call("org.freedesktop.DBus.Properties.Get", 0,
		"org.freedesktop.ModemManager1.Modem", "Ports").Store(&portsVariant); err == nil {
		if ports, ok := portsVariant.Value().([][]interface{}); ok {
			for _, p := range ports {
				if len(p) != 2 {
					continue
				}
				name, _ := p[0].(string)
				typ, _ := p[1].(uint32)
				if typ == mmPortTypeGPS && name != "" {
					return "/dev/" + name
				}
			}
		}
	}
	return defaultNMEAPort
}

// stopReader stops the current nmeaReaderLoop (if any) and closes its serial
// port. Safe to call when no reader is running.
func stopReader() {
	mu.Lock()
	stop := nmeaStop
	f := nmeaFile
	nmeaStop = nil
	mu.Unlock()

	if stop != nil {
		close(stop)
	}
	if f != nil {
		f.Close() // unblocks the scanner inside readNMEALines
	}
}

// watchModemManager reacts to ModemManager InterfacesAdded/InterfacesRemoved
// signals so the package follows the modem across USB re-enumerations instead
// of holding a D-Bus path that has become permanently invalid.
func watchModemManager(conn *dbus.Conn, sigCh chan *dbus.Signal, stop <-chan struct{}) {
	defer conn.RemoveSignal(sigCh)
	for {
		select {
		case <-stop:
			return
		case sig, ok := <-sigCh:
			if !ok {
				return
			}
			handleMMSignal(conn, sig)
		}
	}
}

// mmStateNames/mmStateChangeReasonNames map the MMModemState/
// MMModemStateChangeReason D-Bus enums (org.freedesktop.ModemManager1.Modem's
// StateChanged signal) to the same names ModemManager's own logs use. Falls
// back to the raw number for anything outside the known range, so an
// unrecognized value still logs something useful instead of a wrong label.
var mmStateNames = map[int32]string{
	-1: "failed", 0: "unknown", 1: "initializing", 2: "locked",
	3: "disabled", 4: "disabling", 5: "enabling", 6: "enabled",
	7: "searching", 8: "registered", 9: "disconnecting", 10: "connecting", 11: "connected",
}

func mmStateName(s int32) string {
	if name, ok := mmStateNames[s]; ok {
		return name
	}
	return fmt.Sprintf("state(%d)", s)
}

var mmStateChangeReasonNames = map[uint32]string{
	0: "unknown", 1: "user-requested", 2: "suspend", 3: "failure",
}

func mmStateChangeReasonName(r uint32) string {
	if name, ok := mmStateChangeReasonNames[r]; ok {
		return name
	}
	return fmt.Sprintf("reason(%d)", r)
}

// mm3gppRegStateNames maps the MM_MODEM_3GPP_REGISTRATION_STATE D-Bus enum
// (Modem3gpp's RegistrationState property) to the same names ModemManager's
// own logs use — verified live: a modem reset took RegistrationState from 4
// ("unknown", right when the object first appears) to 1 ("home").
var mm3gppRegStateNames = map[uint32]string{
	0: "idle", 1: "home", 2: "searching", 3: "denied", 4: "unknown", 5: "roaming",
	6: "home-sms-only", 7: "roaming-sms-only", 8: "emergency-only",
	9: "home-csfb-not-preferred", 10: "roaming-csfb-not-preferred", 11: "attached-rlos",
}

func mm3gppRegStateName(s uint32) string {
	if name, ok := mm3gppRegStateNames[s]; ok {
		return name
	}
	return fmt.Sprintf("regstate(%d)", s)
}

func handleMMSignal(conn *dbus.Conn, sig *dbus.Signal) {
	switch sig.Name {
	case "org.freedesktop.ModemManager1.Modem.StateChanged":
		if len(sig.Body) < 3 {
			return
		}
		oldState, ok1 := sig.Body[0].(int32)
		newState, ok2 := sig.Body[1].(int32)
		reason, ok3 := sig.Body[2].(uint32)
		if !ok1 || !ok2 || !ok3 {
			return
		}
		oldName, newName := mmStateName(oldState), mmStateName(newState)
		reasonName := mmStateChangeReasonName(reason)
		msg := fmt.Sprintf("state changed: %s -> %s (%s)", oldName, newName, reasonName)
		fields := map[string]any{"old_state": oldName, "new_state": newName, "reason": reasonName}
		if newState == -1 { // failed
			logger.ErrorFields("modem", msg, fields)
			go logTegrastats("modem entered failed state")
		} else {
			logger.InfoFields("modem", msg, fields)
		}

	case "org.freedesktop.DBus.Properties.PropertiesChanged":
		if len(sig.Body) < 2 {
			return
		}
		iface, ok := sig.Body[0].(string)
		if !ok || iface != "org.freedesktop.ModemManager1.Modem.Modem3gpp" {
			return
		}
		changed, ok := sig.Body[1].(map[string]dbus.Variant)
		if !ok {
			return
		}
		regVariant, has := changed["RegistrationState"]
		if !has {
			return
		}
		regState, ok := regVariant.Value().(uint32)
		if !ok {
			return
		}
		msg := "[modem] 3GPP registration: " + mm3gppRegStateName(regState)
		if opVariant, ok := changed["OperatorName"]; ok {
			if op, ok := opVariant.Value().(string); ok && op != "" {
				msg += " (" + op + ")"
			}
		}
		logger.Info("%s", msg)

	case "org.freedesktop.DBus.ObjectManager.InterfacesAdded":
		if len(sig.Body) < 2 {
			return
		}
		path, ok := sig.Body[0].(dbus.ObjectPath)
		if !ok {
			return
		}
		ifaces, ok := sig.Body[1].(map[string]map[string]dbus.Variant)
		if !ok {
			return
		}
		if _, ok := ifaces["org.freedesktop.ModemManager1.Modem"]; !ok {
			return
		}
		logger.Info("[modem] New modem object appeared (%s) — reattaching", path)
		go logTegrastats("modem reattached after re-enumeration")
		if err := attachModem(conn, path); err != nil {
			logger.Warn("[modem] Reattach failed: %v", err)
		}

	case "org.freedesktop.DBus.ObjectManager.InterfacesRemoved":
		if len(sig.Body) < 2 {
			return
		}
		path, ok := sig.Body[0].(dbus.ObjectPath)
		if !ok {
			return
		}
		removedIfaces, ok := sig.Body[1].([]string)
		if !ok {
			return
		}
		mu.Lock()
		current := modemPath
		mu.Unlock()
		if path != current {
			return
		}
		for _, iface := range removedIfaces {
			if iface == "org.freedesktop.ModemManager1.Modem" {
				logger.Warn("[modem] Modem object removed (USB re-enumeration?) — waiting for it to reappear")
				go logTegrastats("modem object removed")
				mu.Lock()
				modemPath = ""
				mu.Unlock()
				break
			}
		}
	}
}

// Close stops the NMEA reader loop, the ModemManager signal watcher, and
// closes the D-Bus connection. Idempotent.
func Close() {
	stopReader()

	mu.Lock()
	sigStop := mmSignalStop
	dbConn := dbusConn
	mmSignalStop = nil
	dbusConn = nil
	modemPath = ""
	mu.Unlock()

	if sigStop != nil {
		close(sigStop)
	}
	if dbConn != nil {
		dbConn.Close()
	}
}

// nmeaReaderLoop runs readNMEALines in a restart loop, reopening the port on I/O errors.
// Stops permanently when stop is closed.
func nmeaReaderLoop(portPath string, initialFile *os.File, stop <-chan struct{}) {
	f := initialFile
	for {
		mu.Lock()
		nmeaFile = f
		mu.Unlock()

		// Watcher: close f when stop fires to unblock the scanner.
		watchDone := make(chan struct{})
		go func(file *os.File) {
			select {
			case <-stop:
				file.Close()
			case <-watchDone:
			}
		}(f)

		readNMEALines(f)
		close(watchDone)
		f.Close() // idempotent if already closed by watcher

		mu.Lock()
		if nmeaFile == f {
			nmeaFile = nil
		}
		mu.Unlock()

		select {
		case <-stop:
			return
		default:
		}

		logger.Warn("[modem] NMEA reader stopped, reconnecting...")
		for {
			select {
			case <-stop:
				return
			case <-time.After(2 * time.Second):
			}
			var err error
			f, err = os.Open(portPath)
			if err == nil {
				break
			}
			logger.Warn("[modem] NMEA port %s unavailable: %v", portPath, err)
		}
	}
}

// readNMEALines reads GNSS sentences from f into nmeaEpoch (grouped by GGA cycle).
// Returns when f is closed or an I/O error occurs.
func readNMEALines(f *os.File) {
	scanner := bufio.NewScanner(f)

	var pending []string
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if !strings.HasPrefix(line, "$") {
			continue
		}
		if !validNMEA(line) {
			logger.Warn("[modem] NMEA checksum mismatch, skipping: %.80s", line)
			continue
		}

		// Feed chrony (if RC_GPS_SHM_UNIT is configured) as soon as a line
		// carrying an active fix's UTC time is read — independent of, and
		// tighter than, the epoch cache below, which only publishes once the
		// *next* GGA arrives (up to one whole fix cycle later).
		if len(line) >= 6 && line[3:6] == "RMC" {
			receivedAt := time.Now()
			if p, ok := parseRMC(line); ok && !p.Time.IsZero() {
				publishGPSTime(p.Time, receivedAt)
			}
		}

		// New GGA frame = new epoch; publish the previous one first.
		if len(line) >= 6 && line[3:6] == "GGA" && len(pending) > 0 {
			snap := make([]string, len(pending))
			copy(snap, pending)
			nmeaMu.Lock()
			nmeaEpoch = snap
			nmeaLastUpdate = time.Now()
			nmeaMu.Unlock()
			pending = pending[:0]
		}
		pending = append(pending, line)
	}
	// Publish any incomplete epoch accumulated before the reader stopped.
	if len(pending) > 0 {
		nmeaMu.Lock()
		nmeaEpoch = pending
		nmeaLastUpdate = time.Now()
		nmeaMu.Unlock()
	}
	// os.ErrClosed is expected during normal shutdown: Close() closes the serial
	// port to unblock the scanner. Suppress this to avoid confusing log noise.
	if err := scanner.Err(); err != nil && !errors.Is(err, os.ErrClosed) {
		logger.Warn("[modem] NMEA reader error: %v", err)
	}
}

// GetNMEA returns frames from the last complete GPS epoch.
// Returns nil with no error if no epoch has been received yet.
func GetNMEA() ([]string, error) {
	nmeaMu.RLock()
	defer nmeaMu.RUnlock()
	if len(nmeaEpoch) == 0 {
		return nil, nil
	}
	result := make([]string, len(nmeaEpoch))
	copy(result, nmeaEpoch)
	return result, nil
}

// NMEAAge returns time since the last complete GPS epoch (one hour if none received).
func NMEAAge() time.Duration {
	nmeaMu.RLock()
	defer nmeaMu.RUnlock()
	if nmeaLastUpdate.IsZero() {
		return time.Hour
	}
	return time.Since(nmeaLastUpdate)
}

// validNMEA checks the XOR checksum of an NMEA sentence ("$...*HH").
// Returns true if there is no checksum field or the checksum is valid.
func validNMEA(s string) bool {
	star := strings.IndexByte(s, '*')
	if star < 0 || star+3 > len(s) {
		return true // no checksum field – accept
	}
	var chk byte
	for i := 1; i < star; i++ { // XOR bytes between '$' and '*'
		chk ^= s[i]
	}
	want, err := strconv.ParseUint(s[star+1:star+3], 16, 8)
	if err != nil {
		return false
	}
	return chk == byte(want)
}

// accessTechNames maps bit position 0–15 of the AccessTechnologies bitmask to names.
// Source: ModemManager D-Bus API (MM_MODEM_ACCESS_TECHNOLOGY_*).
// Bit 15 (5GNR) was added after go-modemmanager v0.1.4; we define it here.
var accessTechNames = [16]string{
	"pots", "gsm", "gsm-compact", "gprs", "edge",
	"umts", "hsdpa", "hsupa", "hspa", "hspa+",
	"1xrtt", "evdo0", "evdoa", "evdob", "lte", "5gnr",
}

// SignalStats holds modem network information.
type SignalStats struct {
	Quality uint32 // signal quality 0–100 %
	Tech    string // active (highest priority) technology (e.g. "lte", "5gnr")
}

// GetSignalStatsCtx queries ModemManager for the signal quality and active
// network technology.
// Each D-Bus call has an independent 5 s timeout: some ModemManager builds
// query the modem hardware synchronously, which can block for 10–30 s without
// a deadline. The parent ctx cancellation is still propagated — the effective
// deadline is min(ctx deadline, now+5s).
func GetSignalStatsCtx(ctx context.Context) (SignalStats, error) {
	conn, path, ok := currentModem()
	if !ok {
		return SignalStats{}, fmt.Errorf("modem not initialized")
	}

	// Per-call timeout: caps each D-Bus round-trip independently of the parent ctx.
	callCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	obj := conn.Object("org.freedesktop.ModemManager1", path)

	// SignalQuality: D-Bus type (uu) – percent, recent_flag.
	// godbus decodes the struct as []interface{}.
	var sqVariant dbus.Variant
	if err := obj.CallWithContext(callCtx, "org.freedesktop.DBus.Properties.Get", 0,
		"org.freedesktop.ModemManager1.Modem", "SignalQuality").Store(&sqVariant); err != nil {
		return SignalStats{}, err
	}
	var quality uint32
	if sv, ok := sqVariant.Value().([]interface{}); ok && len(sv) > 0 {
		quality, _ = sv[0].(uint32)
	}

	// AccessTechnologies: D-Bus type u – bitmask of active radio technologies.
	// Returns the highest-priority (last set bit) technology name.
	// If the parent ctx is cancelled we propagate it; any other error (including
	// callCtx timeout) is tolerated — Tech stays empty, no bitrate penalty.
	var rawTech uint32
	if err := obj.CallWithContext(callCtx, "org.freedesktop.DBus.Properties.Get", 0,
		"org.freedesktop.ModemManager1.Modem", "AccessTechnologies").Store(&rawTech); err != nil {
		if ctx.Err() != nil {
			return SignalStats{}, ctx.Err()
		}
		// Non-fatal: continue with rawTech == 0 (no tech penalty applied).
	}

	// Keep only the highest-priority technology (last set bit in the bitmask).
	var lastTech string
	for i, name := range accessTechNames {
		if rawTech&(1<<uint(i)) != 0 {
			lastTech = name
		}
	}

	stats := SignalStats{
		Quality: quality,
		Tech:    lastTech,
	}
	statusMu.Lock()
	statusStats = stats
	statusHasStats = true
	statusMu.Unlock()
	return stats, nil
}

// WatchSignalStats is the single source of truth for polling ModemManager's
// SignalQuality/AccessTechnologies over D-Bus. Before this existed,
// WatchConnectivity, WatchHealth, feedback.localStatsLoop (once per streaming
// camera) and modem.RunStream each polled independently on their own ticker —
// with two streaming cameras that was ~2.5 redundant D-Bus round trips per
// second, all asking ModemManager the same two properties. They now all read
// CachedSignalStats instead; this is the only remaining caller of
// GetSignalStatsCtx outside of --debug-modem's standalone diagnostics (which
// never runs alongside these watchers, so has no redundancy to remove).
// Polls at 1 s: the tightest requirement among the callers (modem.RunStream's
// telemetry cadence). Stops when ctx is cancelled.
func WatchSignalStats(ctx context.Context) {
	ticker := time.NewTicker(1 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}

		fresh := false
		if ensureOpen() {
			if _, err := GetSignalStatsCtx(ctx); err == nil {
				fresh = true
			}
		}
		statusMu.Lock()
		statusStatsFresh = fresh
		statusMu.Unlock()
	}
}

// CachedSignalStats returns the signal-stats snapshot from the most recent
// WatchSignalStats poll, without making a D-Bus call. ok mirrors the
// err==nil contract of GetSignalStatsCtx for that poll: false if the modem
// wasn't reachable on the last tick (callers get an honest "no signal" rather
// than a stale reading frozen from before an outage started).
func CachedSignalStats() (stats SignalStats, ok bool) {
	statusMu.RLock()
	defer statusMu.RUnlock()
	if !statusStatsFresh {
		return SignalStats{}, false
	}
	return statusStats, true
}

// ── Status snapshot (for display, e.g. the console dashboard) ──────────────
//
// Never performs I/O: reads values cached by the existing background pollers
// (GetSignalStatsCtx, WatchConnectivity) rather than triggering yet another
// D-Bus round-trip on every dashboard refresh tick.

var (
	statusMu         sync.RWMutex
	statusStats      SignalStats
	statusHasStats   bool
	statusStatsFresh bool // set by WatchSignalStats each tick; see CachedSignalStats
	statusConnected  bool
	statusConnKnown  bool
)

// Status is a point-in-time snapshot of modem connectivity for display.
type Status struct {
	Detected   bool // a modem D-Bus object is currently attached
	Connected  bool // last known internet reachability (see WatchConnectivity)
	ConnKnown  bool // whether Connected has been determined yet
	Recovering bool // a soft-nudge/reset recovery is in flight (see watchdog.go)
	Quality    uint32
	Tech       string
	HasStats   bool // whether Quality/Tech have ever been populated
}

// GetStatus returns the current modem status snapshot. Cheap and safe to
// call every tick from a UI loop.
func GetStatus() Status {
	_, _, detected := currentModem()
	statusMu.RLock()
	stats, hasStats := statusStats, statusHasStats
	connected, connKnown := statusConnected, statusConnKnown
	statusMu.RUnlock()
	return Status{
		Detected:   detected,
		Connected:  connected,
		ConnKnown:  connKnown,
		Recovering: isRecovering(),
		Quality:    stats.Quality,
		Tech:       stats.Tech,
		HasStats:   hasStats,
	}
}

// BitrateAdvisoryFromStats returns the recommended max streaming bitrate (bps)
// based on cellular signal stats. Pre-emptive ceiling complementing reactive SRT ABR:
// caps bitrate when radio capacity is structurally below maxBitrate.
// 5G/LTE: no penalty; HSPA+: 85%; UMTS: 55%; 2G: 20% floor.
// Signal <50%: additional reduction; above 50% SRT ABR is sufficient.
func BitrateAdvisoryFromStats(maxBitrate int, s SignalStats) int {
	tech := strings.ToLower(s.Tech)

	var techFactor float64
	switch {
	case strings.Contains(tech, "5gnr") || strings.Contains(tech, "nr5g"):
		techFactor = 1.0 // 5G NR
	case strings.Contains(tech, "lte"):
		techFactor = 1.0 // 4G LTE — full bitrate allowed
	case strings.Contains(tech, "hspa") || strings.Contains(tech, "hsdpa") || strings.Contains(tech, "hsupa"):
		techFactor = 0.85 // HSPA / HSPA+ (3G+)
	case strings.Contains(tech, "umts"):
		techFactor = 0.55 // UMTS 3G base
	case tech == "":
		techFactor = 1.0 // no data — do not penalise
	default: // GPRS, EDGE, GSM
		techFactor = 0.20
	}

	// Signal modifier: applied below 50%; above that SRT handles transient losses.
	var sigFactor float64
	switch {
	case s.Quality >= 50:
		sigFactor = 1.0
	case s.Quality >= 30:
		sigFactor = 0.85
	default: // < 30 %: very poor signal
		sigFactor = 0.60
	}

	ceiling := int(float64(maxBitrate) * techFactor * sigFactor)
	if minFloor := maxBitrate / 5; ceiling < minFloor {
		ceiling = minFloor // never go below the ABR floor
	}
	return ceiling
}

// WatchConnectivity calls onChange whenever modem internet connectivity
// changes. connected=true when signal quality > 0, a data technology is
// active, and WatchHealth isn't mid-recovery from a stuck data bearer:
// SignalQuality/AccessTechnologies reflect radio registration, not whether
// the data bearer is actually passing traffic, so a registered-but-stuck
// modem would otherwise never be reported as disconnected here (see
// isRecovering in watchdog.go).
// Polls every 2 s (reading the shared signal-stats cache kept fresh by
// WatchSignalStats, not its own D-Bus call). Stops when ctx is cancelled.
// Never gives up permanently if the modem hasn't been detected yet (unlike
// an earlier version of this function) — on the fixed target hardware the
// modem always shows up eventually, and giving up silently disabled stream
// pause/resume for the rest of the process if Open() happened to lose the
// startup race. A later error (e.g. the D-Bus object briefly gone during a
// USB re-enumeration, handled by attachModem in the background) is treated
// as a connectivity drop, not a permanent absence.
func WatchConnectivity(ctx context.Context, onChange func(connected bool)) {
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()

	var last *bool
	var warnOnce sync.Once

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}

		if !ensureOpen() {
			warnOnce.Do(func() {
				logger.Info("[modem] No modem detected yet — stream valve control idle, will keep retrying")
			})
			continue
		}

		stats, ok := CachedSignalStats()
		connected := ok && stats.Quality > 0 && stats.Tech != "" && !isRecovering()
		statusMu.Lock()
		statusConnected = connected
		statusConnKnown = true
		statusMu.Unlock()
		if last == nil || *last != connected {
			last = &connected
			onChange(connected)
		}
	}
}

// Position holds location data extracted from NMEA frames.
type Position struct {
	Lat    float64   // decimal degrees (+ = N)
	Lon    float64   // decimal degrees (+ = E)
	Alt    float64   // altitude MSL in metres (GGA)
	Speed  float64   // speed over ground in knots (RMC)
	Course float64   // true bearing in degrees (RMC)
	HDOP   float64   // horizontal dilution of precision (GGA)
	Sats   int       // number of satellites used (GGA)
	Fix    bool      // true if fix quality > 0 (GGA)
	Time   time.Time // UTC, from RMC's hhmmss.ss + ddmmyy; zero if unavailable
}

// ParseNMEA extracts a Position from a set of NMEA sentences.
// Parses $GPGGA/$GNGGA (position, altitude, HDOP, sats) and $GPRMC/$GNRMC (speed, course).
// Returns false if no usable sentence was found.
func ParseNMEA(sentences []string) (Position, bool) {
	var pos Position
	var hasGGA, hasRMC bool

	for _, s := range sentences {
		s = strings.TrimSpace(s)
		switch {
		case strings.HasPrefix(s, "$GPGGA"), strings.HasPrefix(s, "$GNGGA"):
			if p, ok := parseGGA(s); ok {
				pos.Lat = p.Lat
				pos.Lon = p.Lon
				pos.Alt = p.Alt
				pos.HDOP = p.HDOP
				pos.Sats = p.Sats
				pos.Fix = p.Fix
				hasGGA = true
			}
		case strings.HasPrefix(s, "$GPRMC"), strings.HasPrefix(s, "$GNRMC"):
			if p, ok := parseRMC(s); ok {
				pos.Speed = p.Speed
				pos.Course = p.Course
				pos.Time = p.Time
				hasRMC = true
			}
		}
	}

	return pos, hasGGA || hasRMC
}

// parseGGA parses a $GPGGA/$GNGGA sentence. Returns sats/HDOP even without a fix.
func parseGGA(s string) (Position, bool) {
	if idx := strings.Index(s, "*"); idx >= 0 {
		s = s[:idx]
	}
	f := strings.Split(s, ",")
	if len(f) < 10 {
		return Position{}, false
	}

	quality, _ := strconv.Atoi(f[6])
	sats, _ := strconv.Atoi(f[7])
	hdop, _ := strconv.ParseFloat(f[8], 64)
	alt, _ := strconv.ParseFloat(f[9], 64)

	if quality == 0 || f[2] == "" {
		// No fix: still return sats/HDOP for diagnostics.
		return Position{Sats: sats, HDOP: hdop, Fix: false}, true
	}

	lat, ok := nmeaToDecimal(f[2], f[3])
	if !ok {
		return Position{Sats: sats, HDOP: hdop, Fix: false}, true
	}
	lon, ok := nmeaToDecimal(f[4], f[5])
	if !ok {
		return Position{Sats: sats, HDOP: hdop, Fix: false}, true
	}

	return Position{
		Lat:  lat,
		Lon:  lon,
		Alt:  alt,
		HDOP: hdop,
		Sats: sats,
		Fix:  true,
	}, true
}

// parseRMC parses a $GPRMC/$GNRMC sentence.
// NMEA format: $GPRMC,hhmmss.ss,A,llll.ll,a,yyyyy.yy,a,x.x,x.x,ddmmyy,...
// Returns false if status is 'V' (void, no fix).
func parseRMC(s string) (Position, bool) {
	if idx := strings.Index(s, "*"); idx >= 0 {
		s = s[:idx]
	}
	f := strings.Split(s, ",")
	if len(f) < 10 { // index 9 (ddmmyy) is the field furthest out we read
		return Position{}, false
	}
	if f[2] != "A" { // A = active, V = void
		return Position{}, false
	}

	speed, _ := strconv.ParseFloat(f[7], 64)
	course, _ := strconv.ParseFloat(f[8], 64)
	t, _ := parseNMEATime(f[1], f[9]) // zero time.Time if unparseable — Position.Time then reads as unavailable

	return Position{Speed: speed, Course: course, Time: t}, true
}

// parseNMEATime combines RMC's hhmmss.ss time field and ddmmyy date field
// into a UTC time.Time. NMEA's two-digit year has no century: GPS didn't
// exist before 1980 and this device won't still be recording in 2080, so
// 2000+yy is unambiguous.
func parseNMEATime(hhmmss, ddmmyy string) (time.Time, bool) {
	if len(hhmmss) < 6 || len(ddmmyy) != 6 {
		return time.Time{}, false
	}
	hh, err1 := strconv.Atoi(hhmmss[0:2])
	mm, err2 := strconv.Atoi(hhmmss[2:4])
	secFloat, err3 := strconv.ParseFloat(hhmmss[4:], 64)
	dd, err4 := strconv.Atoi(ddmmyy[0:2])
	mon, err5 := strconv.Atoi(ddmmyy[2:4])
	yy, err6 := strconv.Atoi(ddmmyy[4:6])
	if err1 != nil || err2 != nil || err3 != nil || err4 != nil || err5 != nil || err6 != nil {
		return time.Time{}, false
	}
	sec := int(secFloat)
	nsec := int((secFloat - float64(sec)) * 1e9)
	return time.Date(2000+yy, time.Month(mon), dd, hh, mm, sec, nsec, time.UTC), true
}

// nmeaToDecimal converts NMEA format (DDDMM.MMMM + hemisphere) to decimal degrees.
func nmeaToDecimal(raw, hemi string) (float64, bool) {
	if raw == "" {
		return 0, false
	}
	dotIdx := strings.Index(raw, ".")
	if dotIdx < 2 {
		return 0, false
	}
	deg, err := strconv.ParseFloat(raw[:dotIdx-2], 64)
	if err != nil {
		return 0, false
	}
	min, err := strconv.ParseFloat(raw[dotIdx-2:], 64)
	if err != nil {
		return 0, false
	}
	dec := deg + min/60.0
	if hemi == "S" || hemi == "W" {
		dec = -dec
	}
	return dec, true
}
