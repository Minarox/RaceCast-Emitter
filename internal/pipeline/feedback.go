package pipeline

// feedback.go manages two independent quality mechanisms for streaming cameras:
//
//  1. WatchLocalStats: polls the srtsink element's "stats" GstStructure property
//     directly — RTT, bandwidth and packet-loss are already known locally because
//     the SRT protocol exchanges ACK/NAK messages internally. No network
//     round-trip to the server is needed for adaptive bitrate control.
//
//  2. RunFeedback: opens a lightweight SRT caller connection (streamid
//     "cameraName:feedback") to the server. The receiver sends
//     {"request_idr":true} when its av1parse pipeline emits a decode warning.
//     The emitter reacts by forcing an immediate IDR frame on the encoder.
//     This is the only information that requires a signal from the server side.

// #cgo pkg-config: srt
// #include <srt/srt.h>
// #include <netdb.h>
// #include <string.h>
// #include <stdlib.h>
// #include <stdio.h>
//
// // srt_open_caller connects to host:port in SRT caller mode with the given
// // streamid. SRTO_CONNTIMEO limits the handshake to 5 s; SRTO_RCVTIMEO
// // makes srt_recvmsg return after 2 s of inactivity so the goroutine can
// // check for context cancellation without blocking indefinitely.
// static SRTSOCKET srt_open_caller(const char *host, int port,
//                                  const char *streamid, int latency_ms) {
//     srt_startup();
//     SRTSOCKET s = srt_create_socket();
//     if (s == SRT_INVALID_SOCK) return SRT_INVALID_SOCK;
//
//     int lat = latency_ms;
//     srt_setsockflag(s, SRTO_RCVLATENCY, &lat, sizeof(lat));
//     srt_setsockflag(s, SRTO_STREAMID, streamid, (int)strlen(streamid));
//     int conntimeo = 5000;
//     srt_setsockflag(s, SRTO_CONNTIMEO, &conntimeo, sizeof(conntimeo));
//     int rcvtimeo = 2000;
//     srt_setsockflag(s, SRTO_RCVTIMEO, &rcvtimeo, sizeof(rcvtimeo));
//
//     struct addrinfo hints, *res;
//     memset(&hints, 0, sizeof(hints));
//     hints.ai_family   = AF_UNSPEC;
//     hints.ai_socktype = SOCK_DGRAM;
//     char port_str[8];
//     snprintf(port_str, sizeof(port_str), "%d", port);
//     if (getaddrinfo(host, port_str, &hints, &res) != 0) {
//         srt_close(s); return SRT_INVALID_SOCK;
//     }
//     int rc = srt_connect(s, res->ai_addr, (int)res->ai_addrlen);
//     freeaddrinfo(res);
//     if (rc == SRT_ERROR) { srt_close(s); return SRT_INVALID_SOCK; }
//     return s;
// }
//
// static int srt_fb_recv(SRTSOCKET s, char *buf, int maxlen) {
//     return srt_recvmsg(s, buf, maxlen);
// }
//
// static void srt_fb_close(SRTSOCKET s) { srt_close(s); }
//
// static int srt_fb_invalid(SRTSOCKET s) {
//     return s == SRT_INVALID_SOCK ? 1 : 0;
// }
//
// static int srt_fb_is_timeout(void) {
//     const char *es = srt_getlasterror_str();
//     if (es && strstr(es, "timeout")) return 1;
//     return 0;
// }
import "C"

import (
	"encoding/json"
	"os"
	"strings"
	"time"
	"unsafe"

	"racecast-emitter/internal/logger"
)

// ABR thresholds and parameters.
const (
	fbLocalInterval  = 3 * time.Second
	fbRetryDelay     = 5 * time.Second
	fbLatencyMS      = 500
	fbDecreaseLoss   = 5.0   // % packet loss → reduce bitrate
	fbDecreaseRTT    = 400.0 // ms RTT → reduce bitrate
	fbIncreaseLoss   = 0.5   // % packet loss (below) → allow recovery
	fbIncreaseRTT    = 200.0 // ms RTT (below) → allow recovery
	fbDecreaseFactor = 0.70  // reduce to 70% on degraded link
	fbIncreaseFactor = 1.10  // recover +10% on stable link
	fbStableNeeded   = 3     // consecutive stable samples before increasing
)

// feedbackMsg is the JSON signal sent from the receiver to the emitter when
// the AV1 decoder pipeline emits a non-fatal bitstream warning.
type feedbackMsg struct {
	RequestIDR bool `json:"request_idr"`
}

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

// ── IDR request listener (receiver → emitter) ─────────────────────────────────

// RunFeedback opens an SRT caller connection (streamid "cameraName:feedback")
// and listens for IDR request signals from the receiver. When the receiver
// detects an AV1 decode warning it sends {"request_idr":true}; the emitter
// forces an immediate IDR frame on the encoder.
func (p *GstPipeline) RunFeedback(encoderName, cameraName string) {
	host := strings.TrimSpace(os.Getenv("RC_SRT_HOST"))
	port := srtPortStart()
	if host == "" {
		logger.Warn("[idr:%s] RC_SRT_HOST not set — IDR request listener disabled", cameraName)
		return
	}
	go p.idrLoop(encoderName, cameraName, host, port)
}

func (p *GstPipeline) idrLoop(encoderName, cameraName, host string, port int) {
	streamID := cameraName + ":feedback"
	for {
		select {
		case <-p.ctx.Done():
			return
		default:
		}

		cHost := C.CString(host)
		cStreamID := C.CString(streamID)
		sock := C.srt_open_caller(cHost, C.int(port), cStreamID, C.int(fbLatencyMS))
		C.free(unsafe.Pointer(cHost))
		C.free(unsafe.Pointer(cStreamID))

		if C.srt_fb_invalid(sock) != 0 {
			logger.Warn("[idr:%s] Connection to %s:%d failed — retrying in %s",
				cameraName, host, port, fbRetryDelay)
			select {
			case <-p.ctx.Done():
				return
			case <-time.After(fbRetryDelay):
			}
			continue
		}
		logger.Info("[idr:%s] Connected to %s:%d", cameraName, host, port)
		p.readIDRRequests(sock, encoderName, cameraName)
		C.srt_fb_close(sock)

		select {
		case <-p.ctx.Done():
			return
		case <-time.After(fbRetryDelay):
		}
	}
}

func (p *GstPipeline) readIDRRequests(sock C.SRTSOCKET, encoderName, cameraName string) {
	buf := make([]byte, 256) // IDR signals are small JSON objects

	for {
		select {
		case <-p.ctx.Done():
			return
		default:
		}

		n := C.srt_fb_recv(sock, (*C.char)(unsafe.Pointer(&buf[0])), C.int(len(buf)))
		if n == 0 {
			logger.Info("[idr:%s] Connection closed by receiver", cameraName)
			return
		}
		if n < 0 {
			if C.srt_fb_is_timeout() != 0 {
				continue // SRTO_RCVTIMEO expired, no signal — loop and check ctx
			}
			logger.Warn("[idr:%s] Receive error — reconnecting", cameraName)
			return
		}

		var msg feedbackMsg
		if err := json.Unmarshal(buf[:int(n)], &msg); err != nil {
			continue
		}
		if msg.RequestIDR {
			logger.Info("[idr:%s] IDR forced by receiver (decode warning)", cameraName)
			p.ForceIDR(encoderName)
		}
	}
}
