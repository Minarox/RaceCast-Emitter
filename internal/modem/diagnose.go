package modem

// diagnose.go implements the --debug-modem CLI mode: passive, persistent
// logging of modem events only — kernel USB fault lines (WatchKernelLog),
// ModemManager state transitions (via the StateChanged D-Bus subscription
// already set up in Open/attachModem), and a periodic signal/GPS snapshot
// for context even when nothing changes. Takes no corrective action —
// WatchHealth is deliberately not started here, since this mode exists
// purely to observe, for live debugging during a race without recording,
// streaming, or telemetry running. Replaces the standalone modem_watch.sh
// script: same diagnostic value, but reachable during a live session
// instead of a separate process someone has to remember to start.

import (
	"context"
	"time"

	"racecast-emitter/internal/logger"
)

const diagnosticsSnapshotInterval = 30 * time.Second

// RunDiagnostics attaches to the modem and logs its events until ctx is
// cancelled.
func RunDiagnostics(ctx context.Context) {
	if !waitForModem(ctx) {
		return
	}
	defer Close()

	go WatchKernelLog(ctx)

	logger.Info("[modem] Diagnostics started — logging events only, no recording/streaming/telemetry")

	ticker := time.NewTicker(diagnosticsSnapshotInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			logSnapshot(ctx)
		}
	}
}

// logSnapshot logs one signal/GPS status line, so the log has continuous
// context even during a stretch where nothing eventful happens.
func logSnapshot(ctx context.Context) {
	stats, err := GetSignalStatsCtx(ctx)
	if err != nil {
		logger.Info("[modem] snapshot: signal unavailable (%v)", err)
		return
	}

	sentences, _ := GetNMEA()
	pos, hasPos := ParseNMEA(sentences)
	if hasPos && pos.Fix {
		logger.Info("[modem] snapshot: signal=%d%% %s  fix lat=%.6f lon=%.6f sats=%d",
			stats.Quality, stats.Tech, pos.Lat, pos.Lon, pos.Sats)
	} else {
		logger.Info("[modem] snapshot: signal=%d%% %s  no GPS fix", stats.Quality, stats.Tech)
	}
}
