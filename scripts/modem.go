package scripts

// References:
// https://www.waveshare.com/wiki/RM520N-GL_5G_for_Jetson_Nano
// Quectel RG520N/RG52xF/RM520N/RM530N Series AT Commands Manual

import (
	"errors"
	"math/rand"
	"os"
	"strings"
	"sync"
	"time"

	"go.bug.st/serial"
	"racecast-emitter/utils"
)

// ── Types ─────────────────────────────────────────────────────────────────────

// atConn holds the exclusive serial connection to the modem's AT interface.
type atConn struct {
	mu   sync.Mutex
	port serial.Port
	dead bool // true when the port has been invalidated by a USB disconnect
}

// modemCache holds the latest values written by the background polling goroutine.
// Reads and writes are guarded by mu so GetModemData never blocks on serial I/O.
type modemCache struct {
	mu     sync.RWMutex
	tech   string
	signal *int
	lat    *float32
	lon    *float32
	alt    *float32
	spd    *float32
	sat    *int
	hdop   *float32
}

// ── Package-level state ───────────────────────────────────────────────────────

var (
	conn  *atConn
	cache modemCache
)

// ── Public API ────────────────────────────────────────────────────────────────

// SetupModem opens the AT serial port, configures GNSS, and enables XTRA
// auto-download so the first GPS fix is as fast as possible.
func SetupModem() {
	conn = &atConn{}
	if err := conn.openPort(); err != nil {
		utils.Log.Fatalw("Failed to open modem AT port", "port", atPortPath(), "error", err)
	}
	conn.configure()

	// Poll modem and GPS independently from the main update goroutine so that
	// serial latency (~6 s worst case for two AT commands) does not block the
	// UPS read and LiveKit publish cycle.
	go pollLoop()
}

// openPort opens (or reopens) the serial port after a USB reconnect.
func (c *atConn) openPort() error {
	portPath := atPortPath()
	port, err := serial.Open(portPath, &serial.Mode{
		BaudRate: 115200,
		DataBits: 8,
		StopBits: serial.OneStopBit,
		Parity:   serial.NoParity,
	})
	if err != nil {
		return err
	}
	c.mu.Lock()
	if c.port != nil {
		_ = c.port.Close()
	}
	c.port = port
	c.dead = false
	c.mu.Unlock()
	return nil
}

// configure sends the initial AT commands after (re)opening the port.
func (c *atConn) configure() {
	c.send("ATE0")      // disable echo
	c.send("AT+CMEE=2") // verbose error codes
	c.send("AT&D0")     // ignore DTR — prevents USB reset cascade on port close

	// Enable XTRA assistance before starting GPS so the first fix benefits from
	// the almanac. The modem auto-downloads a fresh file whenever it expires.
	c.send("AT+QGPSXTRA=1")
	c.send("AT+QGPSXTRAAUTODL=1")

	// All GNSS constellations: GPS + GLONASS + BeiDou + Galileo + SBAS + QZSS
	c.send(`AT+QGPSCFG="gnssconfig",7`)

	// Start GPS engine (idempotent: skip if already running)
	if !strings.Contains(c.send("AT+QGPS?"), "+QGPS: 1") {
		c.send("AT+QGPS=1")
	}
}

// GetModemData returns a snapshot of the latest cached modem and GPS data.
// It returns immediately; the cache is kept fresh by the background poll goroutine.
// signal is a percentage (0–100); GPS fields are nil until the first fix.
func GetModemData() map[string]any {
	cache.mu.RLock()
	defer cache.mu.RUnlock()

	data := map[string]any{
		"tech":   cache.tech,
		"signal": cache.signal,
		"lat":    cache.lat,
		"lon":    cache.lon,
		"alt":    cache.alt,
		"spd":    cache.spd,
		"sat":    cache.sat,
		"hdop":   cache.hdop,
	}
	utils.Log.Infow("Modem Data", "payload", data)
	return data
}

// GetFakeModemData returns randomised data for use in fake/demo mode.
func GetFakeModemData() map[string]any {
	baseLat, baseLon := 48.864716, 2.349014 // Paris

	signal := 30 + rand.Intn(71)
	lat := float32(baseLat + (rand.Float64()-0.5)/100.0)
	lon := float32(baseLon + (rand.Float64()-0.5)/100.0)
	alt := float32(5.0 + rand.Float64()*50.0)
	spd := float32(rand.Float64() * 30.0)
	sat := 4 + rand.Intn(9)
	hdop := float32(0.5 + rand.Float64()*2.5)
	tech := []string{"LTE", "5G", "3G"}[rand.Intn(3)]

	return map[string]any{
		"tech":   tech,
		"signal": &signal,
		"lat":    &lat,
		"lon":    &lon,
		"alt":    &alt,
		"spd":    &spd,
		"sat":    &sat,
		"hdop":   &hdop,
	}
}

// ── Background polling ────────────────────────────────────────────────────────

// pollLoop runs in its own goroutine and refreshes the cache once per second.
// It is decoupled from the main update loop so serial latency never delays
// UPS reads or LiveKit publishes.
//
// When the modem USB-resets, send() marks conn.dead=true. pollLoop detects this,
// waits for the device to reappear, reopens the port, and reconfigures the modem.
func pollLoop() {
	for {
		start := time.Now()

		conn.mu.Lock()
		isDead := conn.dead
		conn.mu.Unlock()

		if isDead {
			utils.Log.Warnw("Modem AT port lost, waiting for USB reconnect…")
			waitForDevice(atPortPath())
			if err := conn.openPort(); err != nil {
				utils.Log.Warnw("Reopen failed, retrying", "error", err)
				time.Sleep(2 * time.Second)
				continue
			}
			utils.Log.Infow("Modem AT port reopened, reconfiguring…")
			conn.configure()
		}

		fetchNetworkInfo()
		fetchGPS()
		if elapsed := time.Since(start); elapsed < time.Second {
			time.Sleep(time.Second - elapsed)
		}
	}
}

// waitForDevice blocks until portPath exists in the filesystem (device reappears
// after USB reenumeration). Gives up after 60 s and returns anyway.
func waitForDevice(portPath string) {
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(portPath); err == nil {
			time.Sleep(200 * time.Millisecond) // let the driver finish attaching
			return
		}
		time.Sleep(250 * time.Millisecond)
	}
	utils.Log.Warnw("Timed out waiting for modem device", "port", portPath)
}

// ── Data fetchers ─────────────────────────────────────────────────────────────

// fetchNetworkInfo queries AT+QENG="servingcell" and writes the best available
// technology label and signal percentage into the cache.
func fetchNetworkInfo() {
	resp := conn.send(`AT+QENG="servingcell"`)
	utils.Log.Debugw("AT+QENG raw", "resp", resp)

	var tech string
	var signal *int

	for _, line := range strings.Split(resp, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "+QENG:") {
			continue
		}
		parts := splitCSV(strings.TrimSpace(strings.TrimPrefix(line, "+QENG:")))
		if len(parts) == 0 {
			continue
		}
		rat := strings.Trim(parts[0], `"`)
		t, s := parseRAT(rat, parts)
		tech, signal = chooseBest(tech, signal, t, s)
	}

	if tech != "" {
		cache.mu.Lock()
		cache.tech = tech
		cache.signal = signal
		cache.mu.Unlock()
	}
}

// fetchGPS queries AT+QGPSLOC=2 and writes the current GPS fix into the cache.
// +CME ERROR: 516 (no fix yet) is silently ignored; cached values are preserved.
//
// Response format: <UTC>,<lat>,<lon>,<hdop>,<alt>,<fix>,<cog>,<spkm>,<spkn>,<date>,<nsat>
func fetchGPS() {
	resp := conn.send("AT+QGPSLOC=2")
	for _, line := range strings.Split(resp, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "+QGPSLOC:") {
			continue
		}
		parts := strings.Split(strings.TrimSpace(strings.TrimPrefix(line, "+QGPSLOC:")), ",")
		if len(parts) < 11 {
			continue
		}
		cache.mu.Lock()
		cache.lat = utils.ParseFloat32(parts[1])
		cache.lon = utils.ParseFloat32(parts[2])
		cache.hdop = utils.ParseFloat32(parts[3])
		cache.alt = utils.ParseFloat32(parts[4])
		cache.spd = utils.ParseFloat32(parts[7])
		cache.sat = utils.ParseInt(parts[10])
		cache.mu.Unlock()
		break
	}
}

// ── RAT parsing helpers ───────────────────────────────────────────────────────

// parseRAT extracts the technology label and signal percentage from a +QENG line.
func parseRAT(rat string, parts []string) (tech string, signal *int) {
	switch rat {
	case "NR5G-SA":
		// "NR5G-SA",MCC,MNC,PCI,RSRP,...  RSRP=parts[4], range -140..-44 dBm
		return "5G", signalPct(parts, 4, -140, -44)
	case "NR5G-NSA":
		// "NR5G-NSA",MCC,MNC,PCI,RSRP,...  RSRP=parts[4], range -140..-44 dBm
		return "5G-NSA", signalPct(parts, 4, -140, -44)
	case "LTE":
		// "LTE",duplex,MCC,MNC,cellID,PCI,EARFCN,band,ul_bw,dl_bw,TAC,RSRP,...
		// RSRP=parts[11], range -140..-44 dBm
		return "LTE", signalPct(parts, 11, -140, -44)
	case "WCDMA":
		// "WCDMA",MCC,MNC,LAC,cellID,UARFCN,PSC,RAC,RSCP,...
		// RSCP=parts[8], range -120..-25 dBm
		return "3G", signalPct(parts, 8, -120, -25)
	case "GSM":
		// "GSM",MCC,MNC,LAC,cellID,BSIC,ARFCN,RSSI,...
		// RSSI=parts[7], range -113..-51 dBm
		return "2G", signalPct(parts, 7, -113, -51)
	}
	return "", nil
}

// chooseBest returns the higher-priority (tech, signal) pair.
// Priority order: 5G > 5G-NSA > LTE > 3G > 2G.
func chooseBest(curTech string, curSignal *int, newTech string, newSignal *int) (string, *int) {
	rank := map[string]int{"5G": 5, "5G-NSA": 4, "LTE": 3, "3G": 2, "2G": 1}
	if rank[newTech] > rank[curTech] {
		return newTech, newSignal
	}
	return curTech, curSignal
}

// signalPct extracts a dBm value at parts[idx] and converts it to a 0–100 percentage.
// Returns nil when the index is out of range or the value cannot be parsed.
func signalPct(parts []string, idx, minDBm, maxDBm int) *int {
	if idx >= len(parts) {
		return nil
	}
	v := utils.ParseInt(parts[idx])
	if v == nil {
		return nil
	}
	return dbmToPercent(*v, minDBm, maxDBm)
}

// dbmToPercent linearly maps dbm ∈ [minDBm, maxDBm] to a percentage in [0, 100].
func dbmToPercent(dbm, minDBm, maxDBm int) *int {
	pct := (dbm - minDBm) * 100 / (maxDBm - minDBm)
	if pct < 0 {
		pct = 0
	} else if pct > 100 {
		pct = 100
	}
	return &pct
}

// ── AT serial layer ───────────────────────────────────────────────────────────

// errPortDead is returned by send when the serial port has been invalidated
// by a USB disconnect. pollLoop uses this to trigger reconnection.
var errPortDead = errors.New("modem port dead")

// send writes a command and returns the full response (blocking, up to 3 s).
// On I/O error it marks the port dead and returns an empty string so callers
// that ignore the second return value degrade gracefully.
func (c *atConn) send(cmd string) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.dead {
		return ""
	}
	_ = c.port.ResetInputBuffer()
	_, err := c.port.Write([]byte(cmd + "\r\n"))
	if err != nil {
		utils.Log.Warnw("AT write error, marking port dead", "cmd", cmd, "error", err)
		c.dead = true
		return ""
	}
	return c.readResponse(3 * time.Second)
}

// readResponse accumulates serial data until a final AT result code is seen or
// the timeout elapses.
func (c *atConn) readResponse(timeout time.Duration) string {
	c.port.SetReadTimeout(200 * time.Millisecond)
	var buf strings.Builder
	deadline := time.Now().Add(timeout)
	chunk := make([]byte, 512)
	for time.Now().Before(deadline) {
		n, _ := c.port.Read(chunk)
		if n > 0 {
			buf.Write(chunk[:n])
			if isResponseComplete(buf.String()) {
				break
			}
		}
	}
	return buf.String()
}

// isResponseComplete returns true when buf ends with a recognized AT result code.
func isResponseComplete(buf string) bool {
	last := lastNonEmptyLine(buf)
	return last == "OK" || last == "ERROR" ||
		strings.HasPrefix(last, "+CME ERROR") ||
		strings.HasPrefix(last, "+CMS ERROR")
}

// lastNonEmptyLine returns the last non-whitespace line in s.
func lastNonEmptyLine(s string) string {
	lines := strings.Split(s, "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		if t := strings.TrimSpace(lines[i]); t != "" {
			return t
		}
	}
	return ""
}

// ── Helpers ───────────────────────────────────────────────────────────────────

// atPortPath returns the AT serial port path, falling back to /dev/ttyUSB2.
func atPortPath() string {
	if p := os.Getenv("MODEM_AT_PORT"); p != "" {
		return p
	}
	return "/dev/ttyUSB2"
}

// splitCSV splits a comma-separated AT response, honouring double-quoted fields.
func splitCSV(s string) []string {
	var parts []string
	var cur strings.Builder
	inQuote := false
	for _, c := range s {
		switch {
		case c == '"':
			inQuote = !inQuote
			cur.WriteRune(c)
		case c == ',' && !inQuote:
			parts = append(parts, cur.String())
			cur.Reset()
		default:
			cur.WriteRune(c)
		}
	}
	return append(parts, cur.String())
}
