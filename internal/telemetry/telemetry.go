package telemetry

// telemetry.go manages the single bidirectional SRT telemetry connection.
// The emitter sends telemetry envelopes (UPS, modem/GPS) to the receiver,
// and receives IDR request signals back from the receiver on the same socket.
// Each envelope carries a "type" field so the receiver can route without
// needing separate SRT streamids.

// #cgo pkg-config: srt
// #include <srt/srt.h>
// #include <netdb.h>
// #include <stdio.h>
// #include <string.h>
// #include <stdlib.h>
//
// static SRTSOCKET telem_dial(const char *host, int port, const char *streamid,
//                             int latency, const char *passphrase) {
//     srt_startup();
//     SRTSOCKET s = srt_create_socket();
//     if (s == SRT_INVALID_SOCK) return SRT_INVALID_SOCK;
//
//     int lat = latency;
//     srt_setsockflag(s, SRTO_LATENCY, &lat, sizeof(lat));
//     srt_setsockflag(s, SRTO_STREAMID, streamid, (int)strlen(streamid));
//
//     // DSCP AF41 (0x88): marks telemetry UDP packets as video-related traffic.
//     int tos = 0x88;
//     srt_setsockflag(s, SRTO_IPTOS, &tos, sizeof(tos));
//
//     // SRTO_RCVTIMEO: srt_recvmsg returns SRT_ERROR after 2 s of inactivity
//     // so the receive goroutine can check for context cancellation.
//     int rcvtimeo = 2000;
//     srt_setsockflag(s, SRTO_RCVTIMEO, &rcvtimeo, sizeof(rcvtimeo));
//
//     // Passphrase authentication (AES-256).
//     if (passphrase && strlen(passphrase) >= 10) {
//         srt_setsockflag(s, SRTO_PASSPHRASE, passphrase, (int)strlen(passphrase));
//         int pbkeylen = 32;
//         srt_setsockflag(s, SRTO_PBKEYLEN, &pbkeylen, sizeof(pbkeylen));
//     }
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
	"strings"
	"sync"
	"unsafe"

	"racecast-emitter/internal/logger"
)

// Conn is the single bidirectional SRT telemetry connection (streamid "telemetry").
// Outgoing: typed envelopes {"type":"ups","data":{...}} / {"type":"modem","data":{...}}.
// Incoming: IDR request messages {"type":"idr","camera":"..."} from the receiver.
type Conn struct {
	// immutable after creation
	ctx        context.Context
	streamID   string
	host       string
	port       int
	latency    int
	passphrase string
	onRecv     func([]byte) // called for each incoming message; nil = no receive

	// mutable, protected by mu
	mu          sync.Mutex
	sock        C.SRTSOCKET
	dialed      bool
	recvStarted bool
}

// NewConn creates a Conn for the given streamid.
// Reads RC_SRT_HOST, RC_SRT_PORT (default 9000) and RC_SRT_LATENCY (default 800).
// onRecv is called (in a dedicated goroutine) for each message received from
// the server; pass nil if no incoming messages are expected.
// If RC_SRT_HOST is not set, Send is a no-op.
func NewConn(ctx context.Context, streamID string, onRecv func([]byte)) *Conn {
	return &Conn{
		ctx:        ctx,
		streamID:   streamID,
		host:       os.Getenv("RC_SRT_HOST"),
		port:       envInt("RC_SRT_PORT", 9000),
		latency:    envInt("RC_SRT_LATENCY", 800),
		passphrase: strings.TrimSpace(os.Getenv("RC_SRT_PASSPHRASE")),
		onRecv:     onRecv,
	}
}

// Send sends data over the SRT connection, connecting if necessary.
// No-op (returns nil) if RC_SRT_HOST is not set.
// On send failure the connection is reset; reconnect happens on the next call.
// The mutex is held across the C send call so that recvLoop cannot close the
// socket while a send is in progress (use-after-free prevention).
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
// c.mu must be held by the caller.
func (c *Conn) dialLocked() (C.SRTSOCKET, error) {
	if !c.dialed {
		cHost := C.CString(c.host)
		cSID := C.CString(c.streamID)
		cPass := C.CString(c.passphrase)
		sock := C.telem_dial(cHost, C.int(c.port), cSID, C.int(c.latency), cPass)
		C.free(unsafe.Pointer(cHost))
		C.free(unsafe.Pointer(cSID))
		C.free(unsafe.Pointer(cPass))
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

// recvLoop reads incoming messages from the server and dispatches to onRecv.
// Exits when the connection closes, on error, or when ctx is cancelled.
// SRTO_RCVTIMEO = 2 s means telem_recv unblocks periodically so ctx can fire.
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

