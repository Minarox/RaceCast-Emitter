package ups

// streamer.go sends UPS values to the server every 2 seconds
// via internal/telemetry (streamid "telemetry:ups").

import (
	"context"
	"time"

	"racecast-emitter/internal/logger"
	"racecast-emitter/internal/telemetry"
)

const telemetryInterval = 2 * time.Second

// upsJSON is the JSON payload sent to the server at each interval.
type upsJSON struct {
	V   float64 `json:"v"`   // voltage (V)
	A   float64 `json:"a"`   // current (A)
	W   float64 `json:"w"`   // power (W)
	Pct float64 `json:"pct"` // battery level (%)
}

// waitForUPS blocks until Open succeeds or ctx is cancelled, retrying every
// 3 s. Mirrors modem.waitForModem: a transient "I2C bus not ready yet" race
// at boot must not permanently disable UPS telemetry for the rest of the
// process over one failed Open() call.
func waitForUPS(ctx context.Context) bool {
	if Open() == nil {
		return true
	}
	logger.Warn("[ups] Not detected yet — will keep retrying in the background")
	ticker := time.NewTicker(3 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return false
		case <-ticker.C:
			if Open() == nil {
				logger.Info("[ups] Detected — telemetry starting")
				return true
			}
		}
	}
}

// RunStream reads UPS values every 2 s and records them locally under
// records/<date>/data/ups.jsonl, regardless of send. When send is true, the
// same envelope is also sent to the receiver over conn — false in
// --record-only mode, where conn must never be used even if RC_SRT_HOST
// happens to be configured (that flag's contract is "no SRT streaming").
// Stops when ctx is cancelled.
func RunStream(ctx context.Context, conn *telemetry.Conn, send bool) {
	if !waitForUPS(ctx) {
		return
	}
	defer Close()

	rec := telemetry.NewRecorder("ups")
	defer rec.Close()

	ticker := time.NewTicker(telemetryInterval)
	defer ticker.Stop()

	if send {
		logger.Info("[ups] Telemetry started (recording + SRT)")
	} else {
		logger.Info("[ups] Telemetry started (recording only)")
	}

	srtDown := false
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}

		d, ok := Read()
		if !ok {
			continue // I2C read failed this tick — skip rather than send a corrupted zero reading
		}
		payload, _, err := telemetry.BuildEnvelope("ups", upsJSON{
			V: d.Voltage, A: d.Current, W: d.Power, Pct: d.Percentage,
		})
		if err != nil {
			continue
		}
		if err := rec.Write(payload); err != nil {
			logger.Warn("[ups] Recording telemetry: %v", err)
		}

		if !send {
			continue
		}
		if err := conn.Send(payload); err != nil {
			// Logged only on the connected->down transition: while the receiver
			// is unreachable, Send fails on every tick — including the
			// dialBackoff's own "retry later" error, which is expected and not
			// worth a line per tick.
			if !srtDown {
				srtDown = true
				logger.Warn("[ups] SRT send failing — will keep retrying silently: %v", err)
			}
		} else if srtDown {
			srtDown = false
			logger.Info("[ups] SRT send recovered")
		}
	}
}
