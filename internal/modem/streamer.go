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

	// Delay before the first poll, to allow the initial AGPS fix.
	modemWarmup = 3 * time.Second
)

// modemJSON is the JSON payload sent to the server at each interval.
// GPS fields are zero when no fix is available (fix=false).
type modemJSON struct {
	// GPS
	Lat    float64 `json:"lat"`
	Lon    float64 `json:"lon"`
	Alt    float64 `json:"alt"`
	Speed  float64 `json:"spd"`  // knots
	Course float64 `json:"cog"`  // true degrees
	HDOP   float64 `json:"hdop"`
	Sats   int     `json:"sats"`
	Fix    bool    `json:"fix"`
	NMEA   string  `json:"nmea"` // raw NMEA sentences joined by \r\n
	// Network
	Signal uint32 `json:"signal"` // signal quality 0–100 %
	Tech   string `json:"tech"`   // active technology (e.g. "lte", "lte+nr5g")
}

// RunStream sends modem data every second via conn. Stops when ctx is cancelled.
func RunStream(ctx context.Context, conn *telemetry.Conn) {
	if err := Open(); err != nil {
		logger.Warn("[modem] Telemetry disabled: %v", err)
		return
	}
	defer Close()

	// Wait for the initial AGPS fix before first poll.
	select {
	case <-time.After(modemWarmup):
	case <-ctx.Done():
		return
	}

	ticker := time.NewTicker(modemInterval)
	defer ticker.Stop()

	logger.Info("[modem] Telemetry started")

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			sentences, err := GetNMEA()
			if err != nil {
				logger.Warn("[modem] GetNMEA : %v", err)
			}

			pos, hasPos := ParseNMEA(sentences)
			stats, _ := GetSignalStats()

			data := modemJSON{
				Fix:    hasPos && pos.Fix,
				NMEA:   strings.Join(sentences, "\r\n"),
				Signal: stats.Quality,
				Tech:   stats.Tech,
			}
			if hasPos {
				data.Lat    = pos.Lat
				data.Lon    = pos.Lon
				data.Alt    = pos.Alt
				data.Speed  = pos.Speed
				data.Course = pos.Course
				data.HDOP   = pos.HDOP
				data.Sats   = pos.Sats
			}

			payload, err := json.Marshal(data)
			if err != nil {
				continue
			}

			if err := conn.Send(payload); err != nil {
				logger.Warn("[modem] %v", err)
			}
		}
	}
}
