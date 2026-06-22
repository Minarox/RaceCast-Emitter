package pipeline

// feedback.go manages the local ABR quality mechanism for streaming cameras:
// WatchLocalStats polls the srtsink element's "stats" GstStructure property
// directly — RTT, bandwidth and packet-loss are already known locally because
// the SRT protocol exchanges ACK/NAK messages internally. No network
// round-trip to the server is needed for adaptive bitrate control.
//
// IDR requests from the receiver arrive via the shared bidirectional telemetry
// SRT connection (see internal/telemetry and Slot.ForceIDR in pipeline.go).

import (
	"time"

	"racecast-emitter/internal/logger"
	"racecast-emitter/internal/modem"
)

// ABR thresholds and parameters.
const (
	fbLocalInterval  = 3 * time.Second
	fbDecreaseLoss   = 5.0   // % packet loss → reduce bitrate
	fbDecreaseRTT    = 400.0 // ms RTT → reduce bitrate
	fbIncreaseLoss   = 0.5   // % packet loss (below) → allow recovery
	fbIncreaseRTT    = 200.0 // ms RTT (below) → allow recovery
	fbDecreaseFactor = 0.70  // reduce to 70% on degraded link
	fbIncreaseFactor = 1.10  // recover +10% on stable link
	fbStableNeeded   = 3     // consecutive stable samples before increasing
)

// localStats holds a single interval measurement read from srtsink.
type localStats struct {
	LossPct       float64
	RTTMS         float64
	BandwidthMbps float64
}

// ── Local ABR (no network round-trip) ────────────────────────────────────────

// WatchLocalStats starts a goroutine that reads SRT statistics directly from
// the srtsink GStreamer element every fbLocalInterval and adapts the AV1
// encoder bitrate. The stats (RTT, bandwidth, packet loss) are already known
// locally: the SRT protocol maintains them via its ACK/NAK exchange.
// sinkName must match the name= in the pipeline string (e.g., "srtsink").
func (p *GstPipeline) WatchLocalStats(encoderName, sinkName string, minBitrate, maxBitrate int) {
	go p.localStatsLoop(encoderName, sinkName, minBitrate, maxBitrate)
}

func (p *GstPipeline) localStatsLoop(encoderName, sinkName string, minBitrate, maxBitrate int) {
	ticker := time.NewTicker(fbLocalInterval)
	defer ticker.Stop()

	currentBitrate := maxBitrate
	stableCount := 0
	var prevSent int64
	var prevLost int
	prevEffectiveMax := maxBitrate // tracks last logged modem ceiling

	for {
		select {
		case <-p.ctx.Done():
			return
		case <-ticker.C:
		}

		rttMS, bwMbps, sentTotal, lostTotal := p.GetSRTSinkStats(sinkName)
		if sentTotal == 0 {
			continue // srtsink not yet connected to the server
		}

		// Interval packet-loss rate from cumulative delta.
		deltaSent := sentTotal - prevSent
		deltaLost := lostTotal - prevLost
		prevSent = sentTotal
		prevLost = lostTotal

		var lossPct float64
		if total := float64(deltaSent) + float64(deltaLost); total > 0 {
			lossPct = 100.0 * float64(deltaLost) / total
		}

		// Modem-based pre-emptive ceiling: limits the ABR upper bound based on
		// the cellular technology and signal quality, before SRT stats degrade.
		// When the modem is unavailable the ceiling equals maxBitrate (no effect).
		effectiveMax := maxBitrate
		modemStats, modemErr := modem.GetSignalStats()
		if modemErr == nil {
			effectiveMax = modem.BitrateAdvisoryFromStats(maxBitrate, modemStats)
			if effectiveMax != prevEffectiveMax {
				logger.Info("[abr:%s] Modem ceiling: %d bps (tech=%q signal=%d%%)",
					encoderName, effectiveMax, modemStats.Tech, modemStats.Quality)
				prevEffectiveMax = effectiveMax
			}
			// Enforce the ceiling immediately on a downgrade (e.g. LTE → UMTS)
			// even when SRT stats are still clean.
			if currentBitrate > effectiveMax {
				logger.Info("[abr:%s] Modem ceiling enforced: %d → %d bps",
					encoderName, currentBitrate, effectiveMax)
				p.SetBitrate(encoderName, effectiveMax)
				currentBitrate = effectiveMax
				stableCount = 0
			}
		}

		st := localStats{LossPct: lossPct, RTTMS: rttMS, BandwidthMbps: bwMbps}
		newBitrate := adaptBitrate(currentBitrate, st, minBitrate, effectiveMax, &stableCount)
		if newBitrate != currentBitrate {
			logger.Info("[abr:%s] Bitrate %d → %d bps (loss=%.1f%% rtt=%.0fms bw=%.1fMbps)",
				encoderName, currentBitrate, newBitrate, lossPct, rttMS, bwMbps)
			p.SetBitrate(encoderName, newBitrate)
			currentBitrate = newBitrate
		}
	}
}

// adaptBitrate computes the new bitrate given the current interval stats.
func adaptBitrate(current int, st localStats, min, max int, stableCount *int) int {
	degraded := st.LossPct > fbDecreaseLoss || st.RTTMS > fbDecreaseRTT
	stable := st.LossPct < fbIncreaseLoss && st.RTTMS < fbIncreaseRTT

	switch {
	case degraded:
		*stableCount = 0
		next := int(float64(current) * fbDecreaseFactor)
		if next < min {
			next = min
		}
		return next

	case stable && current < max:
		*stableCount++
		if *stableCount < fbStableNeeded {
			return current
		}
		*stableCount = 0
		next := int(float64(current) * fbIncreaseFactor)
		if next > max {
			next = max
		}
		return next

	default:
		return current
	}
}
