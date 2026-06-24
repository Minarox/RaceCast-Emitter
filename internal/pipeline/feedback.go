package pipeline

// feedback.go manages local ABR for streaming cameras.
// WatchLocalStats polls srtsink's "stats" GstStructure directly (RTT, bandwidth,
// packet-loss are known locally via SRT ACK/NAK — no server round-trip needed).
// IDR requests arrive via the shared bidirectional telemetry connection.

import (
	"time"

	"racecast-emitter/internal/logger"
	"racecast-emitter/internal/modem"
)

// ABR thresholds and parameters.
const (
	fbLocalInterval  = 2 * time.Second
	fbDecreaseLoss   = 3.0   // % packet loss → reduce bitrate
	fbDecreaseRTT    = 150.0 // ms RTT → reduce bitrate
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

// WatchLocalStats starts an ABR goroutine reading SRT stats from sinkName every
// fbLocalInterval and adapting the AV1 encoder bitrate (no server round-trip).
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
	srtConnected := false

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

		// Force an IDR as soon as SRT connects so the server can start
		// decoding immediately without waiting for the next natural keyframe.
		if !srtConnected {
			srtConnected = true
			logger.Info("[abr:%s] SRT connected — forcing IDR", encoderName)
			p.ForceIDR(encoderName)
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

		// Modem ceiling: pre-emptive upper bound based on radio tech/quality,
		// before SRT stats degrade. Equals maxBitrate when modem is unavailable.
		effectiveMax := maxBitrate
		modemStats, modemErr := modem.GetSignalStats()
		if modemErr == nil {
			effectiveMax = modem.BitrateAdvisoryFromStats(maxBitrate, modemStats)
			if effectiveMax != prevEffectiveMax {
				logger.Info("[abr:%s] Modem ceiling: %d bps (tech=%q signal=%d%%)",
					encoderName, effectiveMax, modemStats.Tech, modemStats.Quality)
				prevEffectiveMax = effectiveMax
			}
			// Enforce ceiling immediately on downgrade (e.g. LTE → UMTS).
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
