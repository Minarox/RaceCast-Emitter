package ups

// streamer.go sends UPS values to the server every 2 seconds
// via internal/telemetry (streamid "telemetry:ups").

import (
	"context"
	"encoding/json"
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

// RunStream sends UPS values every 2 s via conn. Stops when ctx is cancelled.
func RunStream(ctx context.Context, conn *telemetry.Conn) {
	if err := Open(); err != nil {
		logger.Warn("[ups] Telemetry disabled: UPS unavailable (%v)", err)
		return
	}

	ticker := time.NewTicker(telemetryInterval)
	defer ticker.Stop()

	logger.Info("[ups] Telemetry started")

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}

		d := Read()
		payload, err := json.Marshal(struct {
			Type string  `json:"type"`
			Data upsJSON `json:"data"`
		}{
			Type: "ups",
			Data: upsJSON{V: d.Voltage, A: d.Current, W: d.Power, Pct: d.Percentage},
		})
		if err != nil {
			continue
		}

		if err := conn.Send(payload); err != nil {
			logger.Warn("[ups] %v", err)
		}
	}
}
