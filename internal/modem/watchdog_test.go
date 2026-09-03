package modem

import (
	"testing"
	"time"
)

func testTuning() watchdogTuning {
	return watchdogTuning{
		failThreshold:     3,
		softGrace:         20 * time.Second,
		resetGrace:        45 * time.Second,
		circuitWindow:     15 * time.Minute,
		maxResetsInWindow: 3,
		circuitBackoff:    10 * time.Minute,
	}
}

func TestDecideAction_StaysReachable(t *testing.T) {
	tune := testTuning()
	st := &watchdogState{}
	now := time.Now()

	for i := 0; i < 10; i++ {
		action, recovered, tripped := decideAction(true, now, st, tune)
		if action != actionNone || recovered || tripped {
			t.Fatalf("tick %d: got action=%v recovered=%v tripped=%v, want actionNone/false/false", i, action, recovered, tripped)
		}
		now = now.Add(10 * time.Second)
	}
}

func TestDecideAction_BelowThresholdStaysIdle(t *testing.T) {
	tune := testTuning()
	st := &watchdogState{}
	now := time.Now()

	for i := 0; i < tune.failThreshold-1; i++ {
		action, _, _ := decideAction(false, now, st, tune)
		if action != actionNone {
			t.Fatalf("tick %d (below threshold): got action=%v, want actionNone", i, action)
		}
		now = now.Add(10 * time.Second)
	}
}

func TestDecideAction_ThresholdTriggersSoftNudge(t *testing.T) {
	tune := testTuning()
	st := &watchdogState{}
	now := time.Now()

	var action healthAction
	for i := 0; i < tune.failThreshold; i++ {
		action, _, _ = decideAction(false, now, st, tune)
		now = now.Add(10 * time.Second)
	}
	if action != actionSoftNudge {
		t.Fatalf("got action=%v on reaching failThreshold, want actionSoftNudge", action)
	}
	if st.stage != stageSoftGrace {
		t.Fatalf("stage = %v, want stageSoftGrace", st.stage)
	}
}

// TestDecideAction_RecoversAfterRadioDropoutDuringSoftGrace covers a gap
// TestDecideAction_RecoversDuringSoftGrace doesn't: WatchHealth's loop calls
// decideAction only when the radio itself is registered (see its own
// `stats.Quality == 0` continue) — if the radio drops out entirely while a
// soft nudge is in flight, decideAction simply isn't called for as long as
// that lasts, leaving st.stage frozen at stageSoftGrace and isRecovering()
// true well past graceUntil. That's fine only because the reachable branch
// at the top of decideAction doesn't consult graceUntil at all — recovery
// must be detected correctly no matter how long that gap was, not just
// while still inside the original grace window.
func TestDecideAction_RecoversAfterRadioDropoutDuringSoftGrace(t *testing.T) {
	tune := testTuning()
	st := &watchdogState{}
	now := time.Now()

	for i := 0; i < tune.failThreshold; i++ {
		action, _, _ := decideAction(false, now, st, tune)
		now = now.Add(10 * time.Second)
		if i == tune.failThreshold-1 && action != actionSoftNudge {
			t.Fatalf("expected actionSoftNudge at threshold, got %v", action)
		}
	}
	if st.stage != stageSoftGrace {
		t.Fatalf("stage after soft nudge = %v, want stageSoftGrace", st.stage)
	}

	// Radio drops out entirely for far longer than softGrace — simulating
	// WatchHealth's continue path, decideAction is simply never called
	// during this gap (state stays exactly as it was).
	now = now.Add(10 * time.Minute)
	if !st.graceUntil.Before(now) {
		t.Fatalf("test setup invalid: graceUntil %v is not before now %v, this isn't exercising the past-grace-window case", st.graceUntil, now)
	}

	// Radio (and the data path) come back — recovery must still be detected
	// correctly even though now is long past the original graceUntil.
	action, recovered, tripped := decideAction(true, now, st, tune)
	if action != actionNone || !recovered || tripped {
		t.Fatalf("recovered after radio dropout: got action=%v recovered=%v tripped=%v, want actionNone/true/false", action, recovered, tripped)
	}
	if st.stage != stageNormal {
		t.Fatalf("stage after recovery = %v, want stageNormal", st.stage)
	}
}

func TestDecideAction_RecoversDuringSoftGrace(t *testing.T) {
	tune := testTuning()
	st := &watchdogState{}
	now := time.Now()

	for i := 0; i < tune.failThreshold; i++ {
		decideAction(false, now, st, tune)
		now = now.Add(10 * time.Second)
	}
	// Still inside the grace window: should not escalate.
	now = now.Add(5 * time.Second)
	action, recovered, tripped := decideAction(false, now, st, tune)
	if action != actionNone || recovered || tripped {
		t.Fatalf("mid-grace still down: got action=%v recovered=%v tripped=%v, want actionNone/false/false", action, recovered, tripped)
	}

	// Now the link comes back while still in grace.
	action, recovered, tripped = decideAction(true, now, st, tune)
	if action != actionNone || !recovered || tripped {
		t.Fatalf("recovered mid-grace: got action=%v recovered=%v tripped=%v, want actionNone/true/false", action, recovered, tripped)
	}
	if st.stage != stageNormal {
		t.Fatalf("stage after recovery = %v, want stageNormal", st.stage)
	}
}

func TestDecideAction_SoftGraceExpiryEscalatesToReset(t *testing.T) {
	tune := testTuning()
	st := &watchdogState{}
	now := time.Now()

	for i := 0; i < tune.failThreshold; i++ {
		decideAction(false, now, st, tune)
		now = now.Add(10 * time.Second)
	}
	// Let the soft-nudge grace period fully elapse without recovery.
	now = now.Add(tune.softGrace + time.Second)
	action, recovered, tripped := decideAction(false, now, st, tune)
	if action != actionReset || recovered || tripped {
		t.Fatalf("after soft grace expiry: got action=%v recovered=%v tripped=%v, want actionReset/false/false", action, recovered, tripped)
	}
	if st.stage != stageResetGrace {
		t.Fatalf("stage = %v, want stageResetGrace", st.stage)
	}
	if len(st.resetTimes) != 1 {
		t.Fatalf("resetTimes = %v, want 1 entry", st.resetTimes)
	}
}

func TestDecideAction_RepeatedResetsTripCircuitBreaker(t *testing.T) {
	tune := testTuning()
	st := &watchdogState{}
	now := time.Now()

	// Reach the first soft nudge.
	for i := 0; i < tune.failThreshold; i++ {
		decideAction(false, now, st, tune)
		now = now.Add(10 * time.Second)
	}
	now = now.Add(tune.softGrace + time.Second)
	action, _, tripped := decideAction(false, now, st, tune) // reset #1
	if action != actionReset || tripped {
		t.Fatalf("reset #1: got action=%v tripped=%v, want actionReset/false", action, tripped)
	}

	// Reset grace expires without recovery -> reset #2.
	now = now.Add(tune.resetGrace + time.Second)
	action, _, tripped = decideAction(false, now, st, tune)
	if action != actionReset || tripped {
		t.Fatalf("reset #2: got action=%v tripped=%v, want actionReset/false", action, tripped)
	}

	// Reset grace expires again -> this is the 3rd reset within the window
	// (maxResetsInWindow=3): still a real Reset attempt (it's the only lever
	// left), but the breaker trips so the caller logs more loudly and no
	// further automated attempt follows until the backoff elapses.
	now = now.Add(tune.resetGrace + time.Second)
	action, _, tripped = decideAction(false, now, st, tune)
	if action != actionReset || !tripped {
		t.Fatalf("reset #3: got action=%v tripped=%v, want actionReset/true (breaker trip)", action, tripped)
	}
	if st.stage != stageCircuitOpen {
		t.Fatalf("stage = %v, want stageCircuitOpen", st.stage)
	}

	// While the breaker is open, further unreachable ticks do nothing.
	now = now.Add(time.Minute)
	action, _, _ = decideAction(false, now, st, tune)
	if action != actionBackoff {
		t.Fatalf("during breaker cooldown: got %v, want actionBackoff", action)
	}

	// After the backoff window elapses, normal evaluation resumes (needs a
	// fresh failThreshold streak before nudging again).
	now = now.Add(tune.circuitBackoff + time.Second)
	action, _, _ = decideAction(false, now, st, tune)
	if action != actionNone {
		t.Fatalf("first tick after breaker cooldown: got %v, want actionNone (streak restarts)", action)
	}
	if st.stage != stageNormal {
		t.Fatalf("stage after cooldown = %v, want stageNormal", st.stage)
	}
}

func TestDecideAction_OldResetsAgeOutOfCircuitWindow(t *testing.T) {
	tune := testTuning()
	st := &watchdogState{}
	now := time.Now()

	// First reset.
	for i := 0; i < tune.failThreshold; i++ {
		decideAction(false, now, st, tune)
		now = now.Add(10 * time.Second)
	}
	now = now.Add(tune.softGrace + time.Second)
	decideAction(false, now, st, tune) // reset #1
	now = now.Add(tune.resetGrace + time.Second)
	decideAction(true, now, st, tune) // recovers cleanly this time

	// Long after the circuit window has fully elapsed, go through the whole
	// ladder again: this should count as reset #1 of a fresh window, not
	// reset #2, since the earlier one aged out.
	now = now.Add(tune.circuitWindow + time.Minute)
	for i := 0; i < tune.failThreshold; i++ {
		decideAction(false, now, st, tune)
		now = now.Add(10 * time.Second)
	}
	now = now.Add(tune.softGrace + time.Second)
	action, _, tripped := decideAction(false, now, st, tune)
	if action != actionReset || tripped {
		t.Fatalf("got action=%v tripped=%v, want actionReset/false (old reset should have aged out of the window)", action, tripped)
	}
	if len(st.resetTimes) != 1 {
		t.Fatalf("resetTimes = %v, want 1 entry (stale one pruned)", st.resetTimes)
	}
}
