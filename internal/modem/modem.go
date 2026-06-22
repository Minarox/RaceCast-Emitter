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

	modemmanager "github.com/maltegrosse/go-modemmanager"

	"racecast-emitter/internal/logger"
)

// defaultNMEAPort is the serial port that emits NMEA frames from the GNSS.
// The Quectel RM520N-GL exposes its GNSS on the second USB serial port (/dev/ttyUSB1).
// Configurable via RC_MODEM_NMEA_PORT.
const defaultNMEAPort = "/dev/ttyUSB1"

var (
	mu   sync.Mutex
	modm modemmanager.Modem
	loc  modemmanager.ModemLocation

	// NMEA cache updated by the background serial reader goroutine.
	// Contains all frames from the last complete GPS epoch.
	nmeaMu    sync.RWMutex
	nmeaEpoch []string

	// Handle to the NMEA serial port, used for clean shutdown.
	nmeaFile *os.File
)

// Open initializes the modem and starts reading NMEA from the serial port.
// Enables gps-unmanaged mode in ModemManager (AT+QGPS=1 for Quectel);
// the GNSS then manages its fixes autonomously and sends NMEA frames on the
// dedicated serial port. Idempotent.
//
// Environment variables:
//   - RC_MODEM_NMEA_PORT: NMEA serial port (default "/dev/ttyUSB1")
func Open() error {
	mu.Lock()
	defer mu.Unlock()

	if modm != nil {
		return nil
	}

	mm, err := modemmanager.NewModemManager()
	if err != nil {
		return fmt.Errorf("ModemManager not found: %w", err)
	}

	modems, err := mm.GetModems()
	if err != nil {
		return fmt.Errorf("GetModems: %w", err)
	}
	if len(modems) == 0 {
		return fmt.Errorf("no modem detected")
	}

	modm = modems[0]

	l, err := modm.GetLocation()
	if err != nil {
		return fmt.Errorf("Location interface unavailable: %w", err)
	}
	loc = l

	// gps-unmanaged: ModemManager sends AT+QGPS=1 but does not capture frames—
	// we read them ourselves from the serial port. signalLocation=false.
	if err := loc.Setup([]modemmanager.MMModemLocationSource{
		modemmanager.MmModemLocationSourceGpsUnmanaged,
	}, false); err != nil {
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
	nmeaFile = f

	go runNMEAReader(f)

	logger.Info("[modem] GPS started (gps-unmanaged, port: %s)", portPath)
	return nil
}

// Close closes the NMEA serial port to unblock and stop the runNMEAReader goroutine.
// Idempotent.
func Close() {
	mu.Lock()
	defer mu.Unlock()
	if nmeaFile != nil {
		nmeaFile.Close()
		nmeaFile = nil
	}
}

// runNMEAReader continuously reads the GNSS serial port and updates nmeaEpoch.
// Frames are grouped by GPS cycle: each new GGA frame starts a new epoch.
func runNMEAReader(f *os.File) {
	defer f.Close()
	scanner := bufio.NewScanner(f)

	var pending []string
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if !strings.HasPrefix(line, "$") {
			continue
		}

		// New GGA frame = start of a new GPS epoch.
		// Publish the previous epoch before starting the new one.
		if len(line) >= 6 && line[3:6] == "GGA" && len(pending) > 0 {
			snap := make([]string, len(pending))
			copy(snap, pending)
			nmeaMu.Lock()
			nmeaEpoch = snap
			nmeaMu.Unlock()
			pending = pending[:0]
		}
		pending = append(pending, line)
	}
	if err := scanner.Err(); err != nil {
		logger.Warn("[modem] NMEA reader stopped: %v", err)
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

// SignalStats holds modem network information.
type SignalStats struct {
	Quality uint32 // signal quality 0–100 %
	Tech    string // active technology (e.g. "lte", "lte+nr5g")
}

// GetSignalStats returns the signal quality and active network technology.
func GetSignalStats() (SignalStats, error) {
	mu.Lock()
	m := modm
	mu.Unlock()

	if m == nil {
		return SignalStats{}, fmt.Errorf("modem not initialized")
	}

	quality, _, err := m.GetSignalQuality()
	if err != nil {
		return SignalStats{}, err
	}

	techs, err := m.GetAccessTechnologies()
	if err != nil {
		return SignalStats{}, err
	}

	parts := make([]string, 0, len(techs))
	for _, t := range techs {
		if s := t.String(); s != "" && s != "unknown" {
			parts = append(parts, s)
		}
	}

	return SignalStats{
		Quality: quality,
		Tech:    strings.Join(parts, "+"),
	}, nil
}

// BitrateAdvisoryFromStats returns the recommended maximum streaming bitrate
// (bps) based on the provided cellular signal statistics. It acts as a
// pre-emptive ceiling that complements the reactive SRT-based ABR: instead of
// waiting for packet loss to appear, it pre-limits the bitrate when the radio
// link capacity is structurally lower than maxBitrate.
//
// Design principles:
//   - 5G NR and 4G LTE never receive a technology penalty (both exceed 12 Mbps).
//   - HSPA / HSPA+ (3G+) receives a small ceiling (~85 %).
//   - UMTS 3G base is capped more aggressively (~55 %).
//   - 2G (GPRS, EDGE) is reduced to the minimum floor (20 %).
//   - Signal quality below 50 % triggers an additional pre-emptive reduction;
//     above 50 % the SRT ABR is sufficient to handle transient losses.
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

	// Signal quality modifier: applied only below 50 %.
	// Above 50 % the SRT layer handles transient losses without help;
	// below 50 % a pre-emptive reduction avoids filling the SRT send buffer.
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

// parseGGA parses a $GPGGA/$GNGGA sentence.
// NMEA format: $GPGGA,hhmmss.ss,llll.ll,a,yyyyy.yy,a,x,xx,x.x,x.x,M,...
func parseGGA(s string) (Position, bool) {
	if idx := strings.Index(s, "*"); idx >= 0 {
		s = s[:idx]
	}
	f := strings.Split(s, ",")
	if len(f) < 10 {
		return Position{}, false
	}

	quality, _ := strconv.Atoi(f[6])

	lat, ok := nmeaToDecimal(f[2], f[3])
	if !ok {
		return Position{}, false
	}
	lon, ok := nmeaToDecimal(f[4], f[5])
	if !ok {
		return Position{}, false
	}

	sats, _ := strconv.Atoi(f[7])
	hdop, _ := strconv.ParseFloat(f[8], 64)
	alt, _ := strconv.ParseFloat(f[9], 64)

	return Position{
		Lat:  lat,
		Lon:  lon,
		Alt:  alt,
		HDOP: hdop,
		Sats: sats,
		Fix:  quality > 0,
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
