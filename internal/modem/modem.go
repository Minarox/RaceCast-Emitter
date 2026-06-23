package modem

import (
	"bufio"
	"context"
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
	mu        sync.Mutex
	modemPath dbus.ObjectPath // D-Bus object path of the detected modem

	// NMEA cache updated by the background serial reader goroutine.
	// Contains all frames from the last complete GPS epoch.
	nmeaMu         sync.RWMutex
	nmeaEpoch      []string
	nmeaLastUpdate time.Time // time of the last published epoch

	// Handle to the current NMEA serial port (set by nmeaReaderLoop).
	nmeaFile *os.File
	// nmeaStop is closed by Close() to terminate nmeaReaderLoop permanently.
	nmeaStop chan struct{}

	dbusConn *dbus.Conn // D-Bus system bus connection
)

// mmLocationSourceGpsUnmanaged = MM_MODEM_LOCATION_SOURCE_GPS_UNMANAGED (bit 4).
const mmLocationSourceGpsUnmanaged = uint32(1 << 4)

// Open initializes the modem and starts the NMEA serial reader.
// Enables gps-unmanaged mode (AT+QGPS=1); the GNSS sends frames autonomously.
// RC_MODEM_NMEA_PORT overrides the default port. Idempotent.
func Open() error {
	mu.Lock()
	defer mu.Unlock()

	if modemPath != "" {
		return nil
	}

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
	dbusConn = conn

	// Locate the first modem via ObjectManager.
	var managed map[dbus.ObjectPath]map[string]map[string]dbus.Variant
	mmObj := conn.Object("org.freedesktop.ModemManager1", "/org/freedesktop/ModemManager1")
	if err := mmObj.Call("org.freedesktop.DBus.ObjectManager.GetManagedObjects", 0).Store(&managed); err != nil {
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
		return fmt.Errorf("no modem detected")
	}
	modemPath = path

	// Enable gps-unmanaged: ModemManager sends AT+QGPS=1 but does not capture frames;
	// we read them from the serial port. signalLocation=false.
	modemObj := conn.Object("org.freedesktop.ModemManager1", modemPath)
	if err := modemObj.Call("org.freedesktop.ModemManager1.Modem.Location.Setup", 0,
		mmLocationSourceGpsUnmanaged, false).Err; err != nil {
		return fmt.Errorf("Location.Setup (gps-unmanaged): %w", err)
	}

	portPath := os.Getenv("RC_MODEM_NMEA_PORT")
	if portPath == "" {
		portPath = defaultNMEAPort
	}

	f, err := os.Open(portPath)
	if err != nil {
		return fmt.Errorf("NMEA port %s: %w", portPath, err)
	}
	nmeaStop = make(chan struct{})
	go nmeaReaderLoop(portPath, f, nmeaStop)

	logger.Info("[modem] GPS started (gps-unmanaged, port: %s)", portPath)
	return nil
}

// Close stops the NMEA reader loop and closes the current serial port. Idempotent.
func Close() {
	mu.Lock()
	stopCh := nmeaStop
	f := nmeaFile
	dbConn := dbusConn
	nmeaStop = nil
	nmeaFile = nil
	dbusConn = nil
	modemPath = ""
	mu.Unlock()

	if stopCh != nil {
		close(stopCh)
	}
	if f != nil {
		f.Close() // unblocks the scanner inside readNMEALines
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
	if err := scanner.Err(); err != nil {
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
	Tech    string // active technology (e.g. "lte", "lte+5gnr")
}

// GetSignalStats returns the signal quality and active network technology.
func GetSignalStats() (SignalStats, error) {
	mu.Lock()
	conn := dbusConn
	path := modemPath
	mu.Unlock()

	if conn == nil || path == "" {
		return SignalStats{}, fmt.Errorf("modem not initialized")
	}

	obj := conn.Object("org.freedesktop.ModemManager1", path)

	// SignalQuality: D-Bus type (uu) – percent, recent_flag.
	// godbus decodes the struct as []interface{}.
	var sqVariant dbus.Variant
	if err := obj.Call("org.freedesktop.DBus.Properties.Get", 0,
		"org.freedesktop.ModemManager1.Modem", "SignalQuality").Store(&sqVariant); err != nil {
		return SignalStats{}, err
	}
	var quality uint32
	if sv, ok := sqVariant.Value().([]interface{}); ok && len(sv) > 0 {
		quality, _ = sv[0].(uint32)
	}

	// AccessTechnologies: D-Bus type u – bitmask of active radio technologies.
	var rawTech uint32
	_ = obj.Call("org.freedesktop.DBus.Properties.Get", 0,
		"org.freedesktop.ModemManager1.Modem", "AccessTechnologies").Store(&rawTech)

	var parts []string
	for i, name := range accessTechNames {
		if rawTech&(1<<i) != 0 {
			parts = append(parts, name)
		}
	}

	return SignalStats{
		Quality: quality,
		Tech:    strings.Join(parts, "+"),
	}, nil
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

// BitrateAdvisory calls GetSignalStats and returns BitrateAdvisoryFromStats.
// When the modem is unavailable, maxBitrate is returned unchanged.
func BitrateAdvisory(maxBitrate int) int {
	stats, err := GetSignalStats()
	if err != nil {
		return maxBitrate
	}
	return BitrateAdvisoryFromStats(maxBitrate, stats)
}

// WatchConnectivity calls onChange whenever modem internet connectivity changes.
// connected=true when signal quality > 0 and a data technology is active.
// Polls every 2 s. Stops when ctx is cancelled.
// If no modem responds after 10 s, assumes no modem on this device and returns
// without calling onChange (stream valves stay in their default open state).
func WatchConnectivity(ctx context.Context, onChange func(connected bool)) {
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()

	var last *bool
	noModemCount := 0

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}

		stats, err := GetSignalStats()
		if err != nil {
			noModemCount++
			if noModemCount >= 5 {
				// 10 s without any modem response — assume no modem on this device.
				logger.Info("[modem] No modem detected — stream valve control disabled")
				return
			}
			continue
		}
		noModemCount = 0

		connected := stats.Quality > 0 && stats.Tech != ""
		if last == nil || *last != connected {
			last = &connected
			onChange(connected)
		}
	}
}

// Run reads and displays modem data every second until ctx is cancelled.
func Run(ctx context.Context) {
	if err := Open(); err != nil {
		logger.Fatal("[modem] Initialization failed: %v", err)
	}
	defer Close()

	logger.Info("[modem] Reading every second (Ctrl+C to stop)")

	ticker := time.NewTicker(1 * time.Second)
	defer ticker.Stop()

	first := true
	for {
		// Check cancellation before any blocking work.
		select {
		case <-ctx.Done():
			return
		default:
		}

		if !first {
			// Move up one line and erase it to overwrite the previous value.
			fmt.Fprint(os.Stdout, "\033[1A\033[2K")
		}

		sentences, _ := GetNMEA()
		pos, hasPos := ParseNMEA(sentences)
		stats, _ := GetSignalStats()

		if hasPos && pos.Fix {
			logger.Info(
				"[modem] fix   lat=%10.6f lon=%11.6f alt=%6.1fm  sats=%2d hdop=%.1f  spd=%5.1fkt cog=%5.1f°  signal=%3d%% %s",
				pos.Lat, pos.Lon, pos.Alt,
				pos.Sats, pos.HDOP,
				pos.Speed, pos.Course,
				stats.Quality, stats.Tech,
			)
		} else {
			logger.Info(
				"[modem] no fix  sats=%2d  signal=%3d%% %s",
				pos.Sats, stats.Quality, stats.Tech,
			)
		}
		first = false

		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// Position holds location data extracted from NMEA frames.
type Position struct {
	Lat    float64 // decimal degrees (+ = N)
	Lon    float64 // decimal degrees (+ = E)
	Alt    float64 // altitude MSL in metres (GGA)
	Speed  float64 // speed over ground in knots (RMC)
	Course float64 // true bearing in degrees (RMC)
	HDOP   float64 // horizontal dilution of precision (GGA)
	Sats   int     // number of satellites used (GGA)
	Fix    bool    // true if fix quality > 0 (GGA)
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
	if len(f) < 9 {
		return Position{}, false
	}
	if f[2] != "A" { // A = active, V = void
		return Position{}, false
	}

	speed, _ := strconv.ParseFloat(f[7], 64)
	course, _ := strconv.ParseFloat(f[8], 64)

	return Position{Speed: speed, Course: course}, true
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
