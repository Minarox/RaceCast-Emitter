package modem

// streamer.go sends modem data (GPS position + network state) to the server
// every second via internal/telemetry (streamid "telemetry:modem").

import (
	"context"
	"encoding/json"
	"strings"
	"time"

	"racecast-emitter/internal/logger"
	"racecast-emitter/internal/telemetry"
)

const (
	modemInterval = 1 * time.Second

	// modemWarmup is the maximum time to wait before the first poll.
	// The wait is skipped early if the NMEA reader already has fresh data
	// (e.g. warm restart after a crash — GPS was already running).
	modemWarmup   = 3 * time.Second
	nmeaFreshAge  = 5 * time.Second // GPS considered live if last epoch < 5 s ago
)

// modemJSON is the JSON payload sent to the server at each interval.
// GPS pointer fields are omitted when there is no fix, so the receiver
// keeps the last known position rather than resetting it to zero.
type modemJSON struct {
	// GPS — nil when no fix; receiver must keep last known position when absent
	Lat    *float64 `json:"lat,omitempty"`
	Lon    *float64 `json:"lon,omitempty"`
	Alt    *float64 `json:"alt,omitempty"`
	Speed  *float64 `json:"spd,omitempty"`  // knots
	Course *float64 `json:"cog,omitempty"`  // true degrees
	HDOP   *float64 `json:"hdop,omitempty"`
	Sats   *int     `json:"sats,omitempty"`
	Fix    bool     `json:"fix"`
	NMEA   *string  `json:"nmea,omitempty"` // raw NMEA sentences joined by \r\n
	// Network — always present
	Signal uint32 `json:"signal"` // signal quality 0–100 %
	Tech   string `json:"tech"`   // active technology (e.g. "lte", "5gnr")
}

// RunStream sends modem data every second via conn. Stops when ctx is cancelled.
func RunStream(ctx context.Context, conn *telemetry.Conn) {
	if err := Open(); err != nil {
		logger.Warn("[modem] Telemetry disabled: %v", err)
		return
	}
	defer Close()

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

	logger.Info("[modem] Telemetry started")

	gpsStale := false
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
			stats, _ := GetSignalStats()

			data := modemJSON{
				Fix:    hasPos && pos.Fix,
				Signal: stats.Quality,
				Tech:   stats.Tech,
			}
			if hasPos && pos.Fix {
				nmea := strings.Join(sentences, "\r\n")
				sats := pos.Sats
				data.Lat    = &pos.Lat
				data.Lon    = &pos.Lon
				data.Alt    = &pos.Alt
				data.Speed  = &pos.Speed
				data.Course = &pos.Course
				data.HDOP   = &pos.HDOP
				data.Sats   = &sats
				data.NMEA   = &nmea
			}

			payload, err := json.Marshal(struct {
				Type string    `json:"type"`
				Data modemJSON `json:"data"`
			}{Type: "modem", Data: data})
			if err != nil {
				continue
			}

			if err := conn.Send(payload); err != nil {
				logger.Warn("[modem] %v", err)
			}
		}
	}
}
