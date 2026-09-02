package modem

// streamer.go sends modem data (GPS position + network state) to the server
// every second via internal/telemetry (streamid "telemetry:modem").

import (
	"context"
	"time"

	"racecast-emitter/internal/logger"
	"racecast-emitter/internal/telemetry"
)

const (
	modemInterval = 1 * time.Second

	// modemWarmup is the maximum time to wait before the first poll.
	// The wait is skipped early if the NMEA reader already has fresh data
	// (e.g. warm restart after a crash — GPS was already running).
	modemWarmup  = 3 * time.Second
	nmeaFreshAge = 5 * time.Second // GPS considered live if last epoch < 5 s ago
)

// modemJSON is the JSON payload sent to the server at each interval.
// GPS pointer fields are omitted when there is no fix, so the receiver
// keeps the last known position rather than resetting it to zero.
//
// Raw NMEA sentences are deliberately NOT included: they encode nothing the
// parsed fields below don't already carry, and doubling the payload every
// second is wasted bandwidth on the same cellular uplink the streaming ABR
// mechanism is trying to protect. Confirmed unused by RaceCast-Receiver,
// which forwards this "data" object opaquely into LiveKit room metadata
// without parsing individual fields.
type modemJSON struct {
	// GPS — nil when no fix; receiver must keep last known position when absent
	Lat    *float64 `json:"lat,omitempty"`
	Lon    *float64 `json:"lon,omitempty"`
	Alt    *float64 `json:"alt,omitempty"`
	Speed  *float64 `json:"spd,omitempty"` // knots
	Course *float64 `json:"cog,omitempty"` // true degrees
	HDOP   *float64 `json:"hdop,omitempty"`
	Sats   *int     `json:"sats,omitempty"`
	Fix    bool     `json:"fix"`
	// Network — always present
	Signal uint32 `json:"signal"` // signal quality 0–100 %
	Tech   string `json:"tech"`   // active technology (e.g. "lte", "5gnr")
}

// waitForModem blocks until ensureOpen succeeds or ctx is cancelled, retrying
// every 3s. On the target hardware the modem is always eventually present —
// ModemManager can just take a few seconds to enumerate it after boot,
// especially now that the modem's power rail is independent of the Jetson's
// (their startup timing is no longer tied together) — so this retries
// indefinitely instead of giving up after one failed Open(), which used to
// disable modem telemetry for the rest of the process over a transient
// startup race. Logs only once so a genuinely modem-less machine (e.g. a dev
// box) doesn't spam.
func waitForModem(ctx context.Context) bool {
	if ensureOpen() {
		return true
	}
	logger.Warn("[modem] Not detected yet — will keep retrying in the background")
	ticker := time.NewTicker(3 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return false
		case <-ticker.C:
			if ensureOpen() {
				logger.Info("[modem] Detected — telemetry starting")
				return true
			}
		}
	}
}

// RunStream reads modem/GPS data every second and records it locally under
// records/<date>/data/gps.jsonl (named "gps", not "modem": the file groups
// by what the data actually is, independent of the "modem" wire type this is
// sent as — a future car-ECU source will get its own dedicated file the same
// way), regardless of send. When send is true, the same envelope is also
// sent to the receiver over conn — false in --record-only mode, where conn
// must never be used even if RC_SRT_HOST happens to be configured (that
// flag's contract is "no SRT streaming"). Stops when ctx is cancelled.
func RunStream(ctx context.Context, conn *telemetry.Conn, send bool) {
	if !waitForModem(ctx) {
		return
	}
	defer Close()

	rec := telemetry.NewRecorder("gps")
	defer rec.Close()

	// Wait for the initial AGPS fix before the first poll, unless the NMEA
	// reader already has fresh data (warm restart — GPS was already active).
	warmup := time.NewTimer(modemWarmup)
	defer warmup.Stop()
	poll := time.NewTicker(200 * time.Millisecond)
	defer poll.Stop()
warmupLoop:
	for {
		select {
		case <-ctx.Done():
			return
		case <-warmup.C:
			break warmupLoop
		case <-poll.C:
			if NMEAAge() < nmeaFreshAge {
				break warmupLoop
			}
		}
	}

	ticker := time.NewTicker(modemInterval)
	defer ticker.Stop()

	if send {
		logger.Info("[modem] Telemetry started (recording + SRT)")
	} else {
		logger.Info("[modem] Telemetry started (recording only)")
	}

	gpsStale := false
	srtDown := false
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if age := NMEAAge(); age > 5*time.Second {
				if !gpsStale {
					logger.Warn("[modem] GPS data stale (%.0fs since last epoch, reader may have stopped)", age.Seconds())
					gpsStale = true
				}
			} else if gpsStale {
				logger.Info("[modem] GPS data recovered")
				gpsStale = false
			}

			sentences, err := GetNMEA()
			if err != nil {
				logger.Warn("[modem] GetNMEA : %v", err)
			}

			pos, hasPos := ParseNMEA(sentences)
			stats, _ := CachedSignalStats()

			data := modemJSON{
				Fix:    hasPos && pos.Fix,
				Signal: stats.Quality,
				Tech:   stats.Tech,
			}
			if hasPos && pos.Fix {
				sats := pos.Sats
				data.Lat = &pos.Lat
				data.Lon = &pos.Lon
				data.Alt = &pos.Alt
				data.Speed = &pos.Speed
				data.Course = &pos.Course
				data.HDOP = &pos.HDOP
				data.Sats = &sats
			}

			payload, _, err := telemetry.BuildEnvelope("modem", data)
			if err != nil {
				continue
			}
			if err := rec.Write(payload); err != nil {
				logger.Warn("[modem] Recording GPS telemetry: %v", err)
			}

			if !send {
				continue
			}
			if err := conn.Send(payload); err != nil {
				// Logged only on the connected->down transition (mirrors gpsStale
				// above): while the receiver is unreachable, Send fails on every
				// tick (1 Hz) — including the dialBackoff's own "retry later"
				// error, which is expected and not worth a line per tick.
				if !srtDown {
					srtDown = true
					logger.Warn("[modem] SRT send failing — will keep retrying silently: %v", err)
				}
			} else if srtDown {
				srtDown = false
				logger.Info("[modem] SRT send recovered")
			}
		}
	}
}
