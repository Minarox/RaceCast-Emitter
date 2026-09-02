package modem

// gpsclock.go feeds GPS-derived UTC time to chrony's SHM refclock driver, so
// the Jetson's system clock can be disciplined against the GPS receiver
// instead of relying solely on network NTP — the cellular link is often
// degraded or absent for stretches during a rally, while a GPS fix, once
// acquired, is self-contained.
//
// This does NOT configure chrony itself. chrony.conf needs a matching line,
// e.g. `refclock SHM 0 poll 4 refid GPS precision 1e-1`, plus
// `makestep 1.0 1` — that last one is deliberate: it allows exactly one clock
// step on the first correction chrony ever makes (typically the moment the
// GPS gets its first fix, since without a battery-backed RTC the system
// clock can otherwise start a run arbitrarily wrong) and only slews every
// correction after that, rather than repeatedly jumping or slowly dragging
// the clock — see CLAUDE.md's Modem section. Both are system configuration,
// out of scope for this repo.

// #include <sys/types.h>
// #include <sys/ipc.h>
// #include <sys/shm.h>
// #include <time.h>
//
// // Mirrors ntpd/gpsd/chrony's well-known "shmTime" struct byte-for-byte
// // (originally from ntpd's refclock_shm.c) — chrony reads this exact layout
// // from a separate process via shared memory, so it must be the real C
// // struct compiled by the platform's own ABI, not a hand-rebuilt Go
// // equivalent that could silently drift from it (e.g. wrong padding).
// struct shmTime {
//     int mode;
//     volatile int count;
//     time_t clockTimeStampSec;
//     int clockTimeStampUSec;
//     time_t receiveTimeStampSec;
//     int receiveTimeStampUSec;
//     int leap;
//     int precision;
//     int nsamples;
//     volatile int valid;
//     unsigned clockTimeStampNSec;
//     unsigned receiveTimeStampNSec;
//     int dummy[8];
// };
//
// // gps_shm_attach attaches the well-known NTP SHM segment for the given unit
// // (creating it if needed — 0x4e545030 + unit is the fixed key chrony/gpsd/
// // ntpd all use for this). Mode 0666: chrony runs as a different,
// // unprivileged user and must be able to read it. Returns NULL on failure
// // (check errno via cgo's built-in errno return).
// static struct shmTime *gps_shm_attach(int unit) {
//     key_t key = 0x4e545030 + unit;
//     int id = shmget(key, sizeof(struct shmTime), IPC_CREAT | 0666);
//     if (id == -1) return NULL;
//     void *addr = shmat(id, NULL, 0);
//     if (addr == (void *)-1) return NULL;
//     return (struct shmTime *)addr;
// }
//
// // gps_shm_write publishes one GPS-derived time sample using the standard
// // mode=1 two-counter write protocol (bump count, write fields, bump count
// // again, then set valid): a concurrent reader (chrony) that sees mismatched
// // counts around its own read knows the sample was caught mid-write and
// // discards it, instead of racing on individual fields.
// static void gps_shm_write(struct shmTime *shm,
//                            long long clockSec, int clockUSec, unsigned clockNSec,
//                            long long recvSec, int recvUSec, unsigned recvNSec,
//                            int precision) {
//     shm->mode = 1;
//     shm->count++;
//     shm->clockTimeStampSec = (time_t)clockSec;
//     shm->clockTimeStampUSec = clockUSec;
//     shm->clockTimeStampNSec = clockNSec;
//     shm->receiveTimeStampSec = (time_t)recvSec;
//     shm->receiveTimeStampUSec = recvUSec;
//     shm->receiveTimeStampNSec = recvNSec;
//     shm->leap = 0;
//     shm->precision = precision;
//     shm->nsamples = 3;
//     shm->count++;
//     shm->valid = 1;
// }
import "C"

import (
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"racecast-emitter/internal/logger"
)

// gpsSHMPrecision is log2(seconds) of the expected accuracy of a sample: NMEA
// sentences carry no sub-second edge signal (that's what a PPS line is for,
// which this device doesn't wire up), so ~0.5s (2^-1) is a conservative,
// honest estimate — chrony weighs this source accordingly rather than
// trusting it as tightly as a PPS-backed one.
const gpsSHMPrecision = -1

var (
	gpsClockOnce sync.Once
	gpsClockSHM  *C.struct_shmTime // nil if disabled, or attach failed
)

// gpsClock lazily attaches the configured SHM segment on first use, so a
// deployment that hasn't set RC_GPS_SHM_UNIT pays no cost and creates no
// segment at all.
func gpsClock() *C.struct_shmTime {
	gpsClockOnce.Do(func() {
		unit, enabled := gpsSHMUnit()
		if !enabled {
			return
		}
		shm, errno := C.gps_shm_attach(C.int(unit))
		if shm == nil {
			logger.Warn("[gps] SHM unit %d: attach failed (%v) — time discipline disabled", unit, errno)
			return
		}
		gpsClockSHM = shm
		logger.Info("[gps] Publishing GPS time to chrony SHM unit %d (needs a matching `refclock SHM %d` in chrony.conf)", unit, unit)
	})
	return gpsClockSHM
}

// gpsSHMUnit reads RC_GPS_SHM_UNIT. Unset/empty disables GPS time discipline
// entirely (the default) — this touches system time, so it's opt-in.
func gpsSHMUnit() (unit int, enabled bool) {
	s := strings.TrimSpace(os.Getenv("RC_GPS_SHM_UNIT"))
	if s == "" {
		return 0, false
	}
	v, err := strconv.Atoi(s)
	if err != nil || v < 0 {
		logger.Warn("[gps] RC_GPS_SHM_UNIT=%q is not a valid non-negative unit — GPS time discipline disabled", s)
		return 0, false
	}
	return v, true
}

// publishGPSTime feeds one GPS-derived time sample to chrony, if configured.
// gpsTime is the UTC time the GPS receiver itself reported (the reference
// "truth"); receivedAt is this process's own clock reading at the moment
// that NMEA line was read — chrony derives the system clock's offset from
// the difference between the two. Called only from the NMEA serial reader
// goroutine, so no locking is needed around the write itself.
func publishGPSTime(gpsTime, receivedAt time.Time) {
	shm := gpsClock()
	if shm == nil {
		return
	}
	gpsTime = gpsTime.UTC()
	receivedAt = receivedAt.UTC()
	C.gps_shm_write(shm,
		C.longlong(gpsTime.Unix()), C.int(gpsTime.Nanosecond()/1000), C.uint(gpsTime.Nanosecond()),
		C.longlong(receivedAt.Unix()), C.int(receivedAt.Nanosecond()/1000), C.uint(receivedAt.Nanosecond()),
		C.int(gpsSHMPrecision),
	)
}
