package modem

// watchdog.go implements the cellular-link health watchdog: WatchHealth
// detects a "stuck bearer" — ModemManager reports the radio attached
// (SignalQuality > 0, a technology active) but no traffic actually gets
// through — and recovers from it. WatchConnectivity alone cannot tell this
// state apart from a genuinely healthy link, because SignalQuality/
// AccessTechnologies reflect radio registration, not bearer/data-path state.
//
// Deliberately probes independent well-known hosts rather than the SRT
// receiver configured in RC_SRT_HOST: a receiver outage (or the SRT server
// simply being down for maintenance) must never trigger a modem reset, since
// resetting the modem cannot fix that and would just needlessly interrupt an
// otherwise-healthy radio session.
//
// Recovery escalates in two steps: a soft reconnect nudge, then a full modem
// reset (via ModemManager — see resetModem). A circuit breaker stops the
// ladder from reset-looping forever against a fault a reset can't fix; past
// that it just logs for manual attention instead of continuing to retry.

import (
	"context"
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	dbus "github.com/godbus/dbus/v5"

	"racecast-emitter/internal/logger"
)

// recovering is set while a soft reconnect nudge is in flight (between
// issuing Modem.Simple.Disconnect and either confirming recovery or
// escalating to a full Reset). WatchConnectivity consults it so it doesn't
// report "connected" purely off radio registration during that window. A
// Reset doesn't need this flag: it tears down and recreates the ModemManager
// object, which already makes GetSignalStatsCtx fail on its own until
// attachModem reattaches — see handleMMSignal's InterfacesRemoved branch.
var recovering atomic.Bool

func setRecovering(v bool) { recovering.Store(v) }
func isRecovering() bool   { return recovering.Load() }

// ── Pure decision core ──────────────────────────────────────────────────
//
// Kept separate from the D-Bus/network side effects below so the escalation
// ladder and circuit breaker can be unit tested without a live modem or
// real time passing (see watchdog_test.go).

type stage int

const (
	stageNormal stage = iota
	stageSoftGrace
	stageResetGrace
	stageCircuitOpen
)

type healthAction int

const (
	actionNone healthAction = iota
	actionSoftNudge
	actionReset
	actionBackoff
)

// watchdogState is decideAction's mutable state, carried across ticks by
// the caller.
type watchdogState struct {
	stage      stage
	failStreak int
	graceUntil time.Time

	resetTimes   []time.Time // sliding window of Reset actions, oldest first
	circuitUntil time.Time
}

type watchdogTuning struct {
	failThreshold     int           // consecutive failed probes before the first action
	softGrace         time.Duration // time to wait after a soft nudge before judging it failed
	resetGrace        time.Duration // time to wait after a Reset before judging it failed
	circuitWindow     time.Duration // rolling window for the reset circuit breaker
	maxResetsInWindow int           // resets within circuitWindow that trips the breaker
	circuitBackoff    time.Duration // how long the breaker stays open once tripped
}

// decideAction is the pure decision core of the health watchdog: given the
// latest probe outcome and the running state, it returns what to do next.
// recovered reports whether this call observed the link come back while an
// active recovery attempt (soft nudge or reset grace) was in flight — the
// caller uses it to clear the "recovering" flag WatchConnectivity consults.
// breakerTripped reports whether this Reset is the one that trips the
// circuit breaker (still worth attempting — it's the only lever left — but
// the caller logs more loudly since no further automated attempt follows
// until the backoff window elapses).
func decideAction(reachable bool, now time.Time, st *watchdogState, tune watchdogTuning) (action healthAction, recovered bool, breakerTripped bool) {
	if st.stage == stageCircuitOpen {
		if now.Before(st.circuitUntil) {
			return actionBackoff, false, false
		}
		// Breaker cooldown elapsed: fall through and re-evaluate this tick
		// fresh, as if starting from stageNormal.
		st.stage = stageNormal
		st.failStreak = 0
	}

	if reachable {
		wasRecovering := st.stage == stageSoftGrace || st.stage == stageResetGrace
		st.stage = stageNormal
		st.failStreak = 0
		return actionNone, wasRecovering, false
	}

	switch st.stage {
	case stageNormal:
		st.failStreak++
		if st.failStreak < tune.failThreshold {
			return actionNone, false, false
		}
		st.stage = stageSoftGrace
		st.graceUntil = now.Add(tune.softGrace)
		st.failStreak = 0
		return actionSoftNudge, false, false

	case stageSoftGrace, stageResetGrace:
		if now.Before(st.graceUntil) {
			return actionNone, false, false // recovery attempt still in flight, give it time
		}
		tripped := escalate(st, now, tune)
		return actionReset, false, tripped

	default: // unreachable, but not one of the above — treat as normal
		st.stage = stageNormal
		return actionNone, false, false
	}
}

// escalate records a new Reset attempt against the circuit-breaker window.
// Reset is always still attempted, even on the attempt that trips the
// breaker — it's cheap and it's the only automated lever available, so
// there's no reason to skip it just because the breaker is about to open.
func escalate(st *watchdogState, now time.Time, tune watchdogTuning) (tripped bool) {
	cutoff := now.Add(-tune.circuitWindow)
	kept := st.resetTimes[:0]
	for _, t := range st.resetTimes {
		if t.After(cutoff) {
			kept = append(kept, t)
		}
	}
	st.resetTimes = append(kept, now)

	if len(st.resetTimes) >= tune.maxResetsInWindow {
		st.stage = stageCircuitOpen
		st.circuitUntil = now.Add(tune.circuitBackoff)
		return true
	}
	st.stage = stageResetGrace
	st.graceUntil = now.Add(tune.resetGrace)
	return false
}

// ── Side effects: reachability probe, D-Bus recovery actions, tuning ────

func defaultTuning() watchdogTuning {
	return watchdogTuning{
		failThreshold:     envInt("RC_MODEM_FAIL_THRESHOLD", 3),
		softGrace:         envSeconds("RC_MODEM_SOFT_GRACE", 20),
		resetGrace:        envSeconds("RC_MODEM_RESET_GRACE", 45),
		circuitWindow:     envSeconds("RC_MODEM_CIRCUIT_WINDOW", 900),
		maxResetsInWindow: envInt("RC_MODEM_CIRCUIT_MAX_RESETS", 3),
		circuitBackoff:    envSeconds("RC_MODEM_CIRCUIT_BACKOFF", 600),
	}
}

func envInt(key string, def int) int {
	if s := os.Getenv(key); s != "" {
		if v, err := strconv.Atoi(s); err == nil && v > 0 {
			return v
		}
	}
	return def
}

func envSeconds(key string, defSeconds int) time.Duration {
	return time.Duration(envInt(key, defSeconds)) * time.Second
}

func modemIface() string {
	if v := strings.TrimSpace(os.Getenv("RC_MODEM_IFACE")); v != "" {
		return v
	}
	return "wwan0"
}

// probeTargets returns the reachability probe destinations. Defaults to two
// well-known, highly-available TLS endpoints on different networks
// (Cloudflare, Google) — raw IPs, no DNS lookup involved, so a resolver
// hiccup can't be mistaken for a dead bearer. Reachable if ANY target
// answers; unreachable requires ALL of them to fail, so one blocked/
// filtered target doesn't cause a false "stuck" verdict.
func probeTargets() []string {
	if v := os.Getenv("RC_MODEM_PROBE_TARGETS"); v != "" {
		var out []string
		for _, p := range strings.Split(v, ",") {
			if p = strings.TrimSpace(p); p != "" {
				out = append(out, p)
			}
		}
		if len(out) > 0 {
			return out
		}
	}
	return []string{"1.1.1.1:443", "8.8.8.8:443"}
}

// probeReachable reports whether the modem's own route can reach the
// internet. The dial's source address is pinned to the modem interface's
// current IPv4 address, not left to the routing table's default choice —
// on a Jetson that also has Wi-Fi up (as it typically does outside a
// stage, for local debugging), an unpinned probe could succeed over Wi-Fi
// while the cellular bearer is actually the one that's stuck, masking
// exactly the fault this watchdog exists to catch.
func probeReachable(ctx context.Context) bool {
	srcIP, err := interfaceIPv4(modemIface())
	if err != nil {
		// Interface down or without an address yet is itself a clear
		// "not reachable via the modem" — no need to spend a dial timeout.
		return false
	}

	timeout := envSeconds("RC_MODEM_PROBE_TIMEOUT", 5)
	dialer := &net.Dialer{
		Timeout:   timeout,
		LocalAddr: &net.TCPAddr{IP: srcIP},
	}
	for _, target := range probeTargets() {
		conn, err := dialer.DialContext(ctx, "tcp", target)
		if err == nil {
			conn.Close()
			return true
		}
	}
	return false
}

func interfaceIPv4(name string) (net.IP, error) {
	ifi, err := net.InterfaceByName(name)
	if err != nil {
		return nil, err
	}
	addrs, err := ifi.Addrs()
	if err != nil {
		return nil, err
	}
	for _, a := range addrs {
		var ip net.IP
		switch v := a.(type) {
		case *net.IPNet:
			ip = v.IP
		case *net.IPAddr:
			ip = v.IP
		}
		if ip4 := ip.To4(); ip4 != nil {
			return ip4, nil
		}
	}
	return nil, fmt.Errorf("interface %s has no IPv4 address", name)
}

// simpleDisconnect tears down the modem's active bearer(s) via
// Modem.Simple.Disconnect (bearer path "/" = all, matching `mmcli
// --simple-disconnect`). The connection is owned by NetworkManager (a gsm
// connection profile with autoconnect + infinite retries — see the .env/
// nmcli setup), so this is a "soft nudge": NM notices the unexpected
// disconnect via ModemManager and re-activates on its own. We don't call
// Simple.Connect ourselves, to avoid fighting NM's own state tracking.
func simpleDisconnect(ctx context.Context) error {
	conn, path, ok := currentModem()
	if !ok {
		return fmt.Errorf("modem not initialized")
	}
	callCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	obj := conn.Object("org.freedesktop.ModemManager1", path)
	return obj.CallWithContext(callCtx, "org.freedesktop.ModemManager1.Modem.Simple.Disconnect", 0, dbus.ObjectPath("/")).Err
}

// resetModem issues a full modem reset (Modem.Reset — what `mmcli -r` calls
// under the hood). The modem re-enumerates on the bus: watchModemManager's
// existing InterfacesRemoved/InterfacesAdded handling reattaches (D-Bus
// object, GPS/NMEA reader) automatically once it's back.
func resetModem(ctx context.Context) error {
	conn, path, ok := currentModem()
	if !ok {
		return fmt.Errorf("modem not initialized")
	}
	callCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	obj := conn.Object("org.freedesktop.ModemManager1", path)
	return obj.CallWithContext(callCtx, "org.freedesktop.ModemManager1.Modem.Reset", 0).Err
}

// ── Main loop ─────────────────────────────────────────────────────────

// WatchHealth is the cellular-link watchdog described at the top of this
// file. Stops when ctx is cancelled. Safe to run alongside WatchConnectivity
// and RunStream — all three independently call ensureOpen() and never block
// each other.
func WatchHealth(ctx context.Context) {
	tune := defaultTuning()
	interval := envSeconds("RC_MODEM_PROBE_INTERVAL", 10)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	st := &watchdogState{}
	var warnOnce sync.Once

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}

		if !ensureOpen() {
			warnOnce.Do(func() {
				logger.Warn("[modem] Health watchdog idle: modem not detected yet")
			})
			continue
		}

		stats, ok := CachedSignalStats()
		if !ok || stats.Quality == 0 || stats.Tech == "" {
			// Radio itself is down or unregistered — WatchConnectivity
			// already owns pausing the stream for this state. Resetting a
			// modem that's simply out of coverage can't create coverage; it
			// would just interrupt a network search already in progress.
			continue
		}

		reachable := probeReachable(ctx)
		action, recovered, breakerTripped := decideAction(reachable, time.Now(), st, tune)

		if recovered {
			setRecovering(false)
			logger.Info("[modem] Data path reachable again")
		}

		switch action {
		case actionSoftNudge:
			logger.WarnFields("modem",
				fmt.Sprintf("Radio OK (signal=%d%% %s) but data path unreachable — nudging reconnect", stats.Quality, stats.Tech),
				map[string]any{"action": "soft_nudge", "consec_fails": st.failStreak, "signal_pct": stats.Quality, "tech": stats.Tech})
			setRecovering(true)
			if err := simpleDisconnect(ctx); err != nil {
				logger.Warn("[modem] Reconnect nudge failed: %v", err)
			}
		case actionReset:
			if breakerTripped {
				logger.ErrorFields("modem",
					fmt.Sprintf("%d resets in the last %s did not restore connectivity — backing off for %s and giving up automated recovery until then",
						tune.maxResetsInWindow, tune.circuitWindow, tune.circuitBackoff),
					map[string]any{"action": "reset", "breaker_tripped": true, "resets_in_window": len(st.resetTimes),
						"window": tune.circuitWindow.String(), "backoff": tune.circuitBackoff.String()})
			} else {
				logger.WarnFields("modem", "Data path still unreachable after reconnect nudge — resetting modem",
					map[string]any{"action": "reset", "breaker_tripped": false, "consec_fails": st.failStreak})
			}
			setRecovering(false) // the Reset's own re-enumeration now owns the outage signal
			if err := resetModem(ctx); err != nil {
				logger.Warn("[modem] Modem reset failed: %v", err)
			}
		}
	}
}
