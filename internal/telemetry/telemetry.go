package telemetry

// telemetry.go manages the single bidirectional SRT telemetry connection.
// The emitter sends typed envelopes (UPS, modem/GPS) to the receiver and
// receives IDR requests back on the same socket.

// #cgo pkg-config: srt
// #include <srt/srt.h>
// #include <netdb.h>
// #include <stdio.h>
// #include <string.h>
// #include <stdlib.h>
//
// static SRTSOCKET telem_dial(const char *host, int port, const char *streamid,
//                             int latency) {
//     srt_startup();
//     SRTSOCKET s = srt_create_socket();
//     if (s == SRT_INVALID_SOCK) return SRT_INVALID_SOCK;
//
//     // SRTO_TRANSTYPE must be set first: it resets all socket options to
//     // their mode-specific defaults. SRTT_LIVE enables srt_sendmsg/recvmsg
//     // (message API) and appropriate live-streaming buffer behaviour.
//     int transtype = SRTT_LIVE;
//     srt_setsockflag(s, SRTO_TRANSTYPE, &transtype, sizeof(transtype));
//
//     int lat = latency;
//     srt_setsockflag(s, SRTO_LATENCY, &lat, sizeof(lat));
//     srt_setsockflag(s, SRTO_STREAMID, streamid, (int)strlen(streamid));
//
//     // DSCP AF41 (0x88): marks telemetry UDP packets as video-related traffic.
//     int tos = 0x88;
//     srt_setsockflag(s, SRTO_IPTOS, &tos, sizeof(tos));
//
//     // SRTO_RCVTIMEO: unblocks recvmsg after 2 s for ctx cancellation check.
//     int rcvtimeo = 2000;
//     srt_setsockflag(s, SRTO_RCVTIMEO, &rcvtimeo, sizeof(rcvtimeo));
//
//     // SRTO_SNDTIMEO: without this, a degraded-but-not-yet-broken connection
//     // (SRT hasn't hit its own peer-idle timeout yet) leaves sendmsg blocking
//     // synchronously for however long that takes — while c.mu is held, which
//     // stalls the other telemetry source (modem/ups share this one Conn).
//     // Bounding it here makes that stall predictable and short instead.
//     int sndtimeo = 2000;
//     srt_setsockflag(s, SRTO_SNDTIMEO, &sndtimeo, sizeof(sndtimeo));
//
//     struct addrinfo hints, *res = NULL;
//     memset(&hints, 0, sizeof(hints));
//     hints.ai_family   = AF_UNSPEC;
//     hints.ai_socktype = SOCK_DGRAM;
//     char portbuf[16];
//     snprintf(portbuf, sizeof(portbuf), "%d", port);
//     if (getaddrinfo(host, portbuf, &hints, &res) != 0 || !res) {
//         srt_close(s);
//         return SRT_INVALID_SOCK;
//     }
//     int rc = srt_connect(s, res->ai_addr, (int)res->ai_addrlen);
//     freeaddrinfo(res);
//     if (rc == SRT_ERROR) {
//         srt_close(s);
//         return SRT_INVALID_SOCK;
//     }
//     return s;
// }
//
// static int telem_send(SRTSOCKET s, const char *data, int len) {
//     return srt_sendmsg(s, data, len, -1, 1);
// }
//
// static int telem_recv(SRTSOCKET s, char *buf, int maxlen) {
//     return srt_recvmsg(s, buf, maxlen);
// }
//
// static void telem_close(SRTSOCKET s) {
//     srt_close(s);
// }
//
// static int telem_invalid(SRTSOCKET s) {
//     return s == SRT_INVALID_SOCK ? 1 : 0;
// }
//
// static int telem_errno_is_timeout(void) {
//     return srt_getlasterror(NULL) == SRT_ETIMEOUT ? 1 : 0;
// }
import "C"

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"sync"
	"time"
	"unsafe"

	"racecast-emitter/internal/logger"
)

// dialBackoff is the minimum interval between two blocking srt_connect
// attempts. Without it, a down receiver makes every failed Send() (from
// either modem.RunStream at 1 Hz or ups.RunStream at 0.5 Hz — both share
// this one Conn and its mutex) retry the dial immediately, and srt_connect
// can block for its own multi-second timeout while holding c.mu — stalling
// the other telemetry source's Send behind it. This caps how often that
// blocking call is even attempted.
const dialBackoff = 3 * time.Second

// Conn is the single bidirectional SRT telemetry connection (streamid "telemetry").
// Outgoing: typed envelopes {"type":"ups",...} / {"type":"modem",...}.
// Incoming: IDR requests {"type":"idr","camera":"..."} from the receiver.
type Conn struct {
	// immutable after creation
	ctx      context.Context
	streamID string
	host     string
	port     int
	latency  int
	onRecv   func([]byte) // called for each incoming message; nil = no receive

	// mutable, protected by mu
	mu              sync.Mutex
	sock            C.SRTSOCKET
	dialed          bool
	recvStarted     bool
	lastDialAttempt time.Time // zero until the first attempt; see dialBackoff
}

// NewConn creates a Conn for streamID, reading RC_SRT_HOST/PORT/LATENCY.
// onRecv is called per incoming message (nil = no receive). No-op if RC_SRT_HOST unset.
func NewConn(ctx context.Context, streamID string, onRecv func([]byte)) *Conn {
	return &Conn{
		ctx:      ctx,
		streamID: streamID,
		host:     os.Getenv("RC_SRT_HOST"),
		port:     envInt("RC_SRT_PORT", 9000),
		latency:  envInt("RC_SRT_LATENCY", 800),
		onRecv:   onRecv,
	}
}

// Send sends data over the SRT connection, dialing if needed.
// No-op if RC_SRT_HOST is unset. Resets the connection on send failure.
func (c *Conn) Send(data []byte) error {
	if c.host == "" || len(data) == 0 {
		return nil
	}
	c.mu.Lock()
	sock, err := c.dialLocked()
	if err != nil {
		c.mu.Unlock()
		return err
	}
	ret := C.telem_send(sock, (*C.char)(unsafe.Pointer(&data[0])), C.int(len(data)))
	if int(ret) < 0 {
		errStr := C.GoString(C.srt_getlasterror_str())
		c.resetLocked(sock)
		c.mu.Unlock()
		return fmt.Errorf("SRT send: %s", errStr)
	}
	c.mu.Unlock()
	return nil
}

// SendStreamClose notifies the receiver that the given stream is being closed
// intentionally (camera disconnect or program shutdown). The receiver uses this
// to skip the reconnect grace period and unpublish the LiveKit track immediately.
// streamKey must be the same "name:source" identity used for that stream's SRT
// streamid (see pipeline.StreamKey) — not just the bare device name, since a
// camera and microphone may share Name and the receiver keys its state by the
// full identity to tell them apart.
// Best-effort: errors are silently ignored (the receiver falls back to the full
// grace timeout if the signal is not delivered).
func (c *Conn) SendStreamClose(streamKey string) error {
	return c.Send(streamCloseMessage(streamKey))
}

// streamCloseMessage builds the JSON payload SendStreamClose sends — split
// out from it so the message shape is testable without a live SRT socket.
func streamCloseMessage(streamKey string) []byte {
	return fmt.Appendf(nil, `{"type":"stream_close","stream":%q}`, streamKey)
}

// SendFrameTime notifies the receiver of one video frame's real capture
// time over a dedicated side-channel Conn (streamid "frametime" — see
// main.go), instead of embedding it in the video bitstream itself: srtsink
// silently splits any buffer over its ~1316-byte default SRT payload size
// into multiple separate wire messages, which corrupts an in-band
// byte-prefix trick for every frame split that way (virtually every
// encoded video frame — this is exactly what RaceCast-Receiver's CLAUDE.md
// "GStreamer bridge" section documents as the actual cause of video never
// decoding). Audio frames stay well under that threshold and keep the
// older, still-correct in-band mechanism (AttachTimestampPrefix) — this is
// video-only. Best-effort: errors are silently ignored, matching
// SendStreamClose — the receiver falls back to measuring wall-clock
// arrival gaps if a frametime message isn't delivered (see
// RaceCast-Receiver's gst.go).
func (c *Conn) SendFrameTime(streamKey string, seq uint64, capturedAt time.Time) error {
	return c.Send(frameTimeMessage(streamKey, seq, capturedAt))
}

// frameTimeMessage builds the JSON payload SendFrameTime sends — split out
// from it so the message shape is testable without a live SRT socket.
func frameTimeMessage(streamKey string, seq uint64, capturedAt time.Time) []byte {
	return fmt.Appendf(nil, `{"type":"frametime","stream":%q,"seq":%d,"ts":%q}`,
		streamKey, seq, capturedAt.UTC().Format(time.RFC3339Nano))
}

// IsConnected reports whether the SRT socket is currently dialed. Cheap
// (mutex-protected bool read, no I/O) — safe to poll from a console dashboard.
func (c *Conn) IsConnected() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.dialed
}

// Configured reports whether RC_SRT_HOST was set (i.e. this Conn is anything
// but a permanent no-op).
func (c *Conn) Configured() bool {
	return c.host != ""
}

// Close closes the SRT socket. Must be called at program shutdown.
func (c *Conn) Close() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.dialed {
		C.telem_close(c.sock)
		c.dialed = false
		c.recvStarted = false
	}
}

// dialLocked dials if not connected and starts the receive goroutine once.
// c.mu must be held.
func (c *Conn) dialLocked() (C.SRTSOCKET, error) {
	if !c.dialed {
		if !c.lastDialAttempt.IsZero() {
			if since := time.Since(c.lastDialAttempt); since < dialBackoff {
				return 0, fmt.Errorf("SRT dial backoff: retrying in %s", (dialBackoff - since).Round(time.Millisecond))
			}
		}
		c.lastDialAttempt = time.Now()

		cHost := C.CString(c.host)
		cSID := C.CString(c.streamID)
		sock := C.telem_dial(cHost, C.int(c.port), cSID, C.int(c.latency))
		C.free(unsafe.Pointer(cHost))
		C.free(unsafe.Pointer(cSID))
		if C.telem_invalid(sock) != 0 {
			return 0, fmt.Errorf("SRT connection failed to %s:%d (streamid=%s)",
				c.host, c.port, c.streamID)
		}
		c.sock = sock
		c.dialed = true
		c.recvStarted = false
		logger.Info("[telemetry] Connected to %s:%d (streamid=%s)", c.host, c.port, c.streamID)
	}

	if !c.recvStarted && c.onRecv != nil {
		c.recvStarted = true
		go c.recvLoop(c.sock)
	}

	return c.sock, nil
}

// resetLocked closes the socket and marks the connection as disconnected.
// c.mu must be held. No-op if sock no longer matches the current active socket.
func (c *Conn) resetLocked(sock C.SRTSOCKET) {
	if c.dialed && c.sock == sock {
		C.telem_close(sock)
		c.dialed = false
		c.recvStarted = false
	}
}

// resetSocket is the public variant of resetLocked for callers that do not hold mu.
func (c *Conn) resetSocket(sock C.SRTSOCKET) {
	c.mu.Lock()
	c.resetLocked(sock)
	c.mu.Unlock()
}

// recvLoop reads incoming messages and dispatches to onRecv.
// SRTO_RCVTIMEO=2s lets telem_recv unblock periodically so ctx can fire.
func (c *Conn) recvLoop(sock C.SRTSOCKET) {
	buf := make([]byte, 4096)

	for {
		if c.ctx.Err() != nil {
			return
		}
		n := C.telem_recv(sock, (*C.char)(unsafe.Pointer(&buf[0])), C.int(len(buf)))
		if n < 0 {
			if C.telem_errno_is_timeout() != 0 {
				continue // SRTO_RCVTIMEO expired — check ctx and loop
			}
			c.resetSocket(sock)
			return
		}
		if n == 0 {
			c.resetSocket(sock)
			return
		}
		c.onRecv(buf[:int(n)])
	}
}

func envInt(key string, def int) int {
	if s := os.Getenv(key); s != "" {
		if v, err := strconv.Atoi(s); err == nil && v >= 0 {
			return v
		}
	}
	return def
}
