package telemetry

// telemetry.go manages SRT telemetry connections to the receiver.
// Each Conn corresponds to a distinct SRT streamid, allowing the receiver
// to route streams without parsing data (e.g. "telemetry:ups", "telemetry:modem").
// The connection is established lazily and re-established after each error.

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
//
//     srt_setsockflag(s, SRTO_STREAMID, streamid, (int)strlen(streamid));
//
//     // DSCP AF41 (0x88): marks telemetry UDP packets as video-related traffic.
//     int tos = 0x88;
//     srt_setsockflag(s, SRTO_IPTOS, &tos, sizeof(tos));
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
// static void telem_close(SRTSOCKET s) {
//     srt_close(s);
// }
//
// static int telem_invalid(SRTSOCKET s) {
//     return s == SRT_INVALID_SOCK ? 1 : 0;
// }
import "C"

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"
	"unsafe"

	"racecast-emitter/internal/logger"
)

// Conn represents an SRT telemetry connection identified by a streamid.
type Conn struct {
	streamID   string
	host       string
	port       int
	latency    int
	passphrase string
	mu         sync.Mutex
	sock       C.SRTSOCKET
	dialed     bool
}

// NewConn creates a Conn for the given streamid.
// Reads RC_SRT_HOST, RC_SRT_PORT (default 9000) and RC_SRT_LATENCY (default 2000).
// If RC_SRT_HOST is not set, Send is a no-op.
func NewConn(streamID string) *Conn {
	return &Conn{
		streamID:   streamID,
		host:       os.Getenv("RC_SRT_HOST"),
		port:       envInt("RC_SRT_PORT", 9000),
		latency:    envInt("RC_SRT_LATENCY", 800),
		passphrase: strings.TrimSpace(os.Getenv("RC_SRT_PASSPHRASE")),
	}
}

// Send sends data over the SRT connection, connecting if necessary.
// No-op (returns nil) if RC_SRT_HOST is not set.
// On send failure the connection is closed; the caller retries on the next interval.
func (c *Conn) Send(data []byte) error {
	if c.host == "" {
		return nil
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	if !c.dialed {
		cHost := C.CString(c.host)
		cSID := C.CString(c.streamID)
		cPass := C.CString(c.passphrase)
		sock := C.telem_dial(cHost, C.int(c.port), cSID, C.int(c.latency), cPass)
		C.free(unsafe.Pointer(cHost))
		C.free(unsafe.Pointer(cSID))
		C.free(unsafe.Pointer(cPass))
		if C.telem_invalid(sock) != 0 {
			return fmt.Errorf("SRT connection failed to %s:%d (streamid=%s)", c.host, c.port, c.streamID)
		}
		c.sock = sock
		c.dialed = true
		logger.Info("[telemetry] Connected to %s:%d (streamid=%s)", c.host, c.port, c.streamID)
	}

	if len(data) == 0 {
		return nil
	}
	ret := C.telem_send(c.sock, (*C.char)(unsafe.Pointer(&data[0])), C.int(len(data)))
	if int(ret) < 0 {
		errStr := C.GoString(C.srt_getlasterror_str())
		C.telem_close(c.sock)
		c.dialed = false
		return fmt.Errorf("SRT send: %s", errStr)
	}
	return nil
}

// Close closes the SRT socket. Must be called at program shutdown.
func (c *Conn) Close() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.dialed {
		C.telem_close(c.sock)
		c.dialed = false
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
