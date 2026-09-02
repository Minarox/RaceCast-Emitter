package pipeline

// bandwidth.go coordinates video bitrate — and, as a second lever once
// bitrate alone isn't enough, resolution — across every camera streaming at
// once, replacing feedback.go's old per-stream-only ABR ceiling for video:
// each stream used to compute modem.BitrateAdvisoryFromStats(itsOwnMax, ...)
// independently, meaning N cameras each aimed for up to their own full
// configured max with zero awareness that they share one uplink — three
// 8Mbps cameras on a clean LTE link would each independently target 8Mbps,
// a 24Mbps aggregate demand with no relationship to what the link can
// actually carry. It also meant every camera discovered congestion on its
// own schedule and could all ramp back up together afterward, re-creating
// the congestion they'd each individually backed off from.
//
// BandwidthCoordinator instead tracks one shared budget for all cameras
// combined, adapts it from the worst-case stats across all of them (so any
// one camera's congestion signal backs off the whole pool, not just itself),
// and allocates it every tick: the camera marked `main` in devices.yaml
// always gets first claim and is never paused, no matter how little is left
// for everyone else; secondary cameras split what remains and are paused
// entirely — not just driven down to an unwatchably low bitrate — once the
// budget can't even cover their configured floor.
//
// Audio is deliberately not part of this: its combined bandwidth footprint
// (tens to a couple hundred kbps per mic) is a rounding error next to
// video's multi-Mbps range, and audio streams keep using feedback.go's
// WatchLocalStats exactly as before.

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"time"

	"racecast-emitter/internal/logger"
	"racecast-emitter/internal/modem"
)

const (
	// tierDowngradeTicks/tierUpgradeTicks are consecutive-tick thresholds
	// (fbLocalInterval=2s each) before switching resolution tier, counted
	// only once a camera is already pinned at its own bitrate floor — the
	// bitrate ABR's own ramp-down (fbDecreaseFactor=0.70/tick) already takes
	// ~5 ticks (~10s) under sustained degradation before floor is reached,
	// so this is on top of that, not instead of it. Deliberately still
	// coarser than the bitrate ABR's own hysteresis (fbStableNeeded=3
	// samples): even though a tier switch is a brief, glitch-free live
	// encoder resolution change (~60ms, measured — see CLAUDE.md) rather
	// than a silent bitrate number changing, the resolution itself visibly
	// changes, so it must not fire on a single bad or good sample. Slower to
	// recover (5 ticks, ~10s) than to degrade (2 ticks, ~4s) for the same
	// reason a misjudged recovery costs another visible switch to undo —
	// but both were shortened from the original 8/5-tick thresholds (set
	// when a switch was assumed to cost far more) once the real ~60ms/
	// zero-dropped-frame cost was measured: a camera pinned at its bitrate
	// floor is already a strong degradation signal on its own, worth
	// reacting to quickly for visibility during poor reception.
	tierDowngradeTicks = 2
	tierUpgradeTicks   = 5

	// tierScale is how much BuildVideoStreamStr's stream resolution is
	// reduced at tier 1 (the only reduced tier implemented). ~0.65 linear
	// scale is roughly 40% of the original pixel count — a real quality
	// improvement at a given bitrate without dropping so far it looks like a
	// different, oddly-cropped camera. Framerate is left untouched:
	// build.go's idrinterval math is framerate-derived, and resolution
	// alone already delivers the bulk of the benefit for meaningfully less
	// risk than also varying framerate.
	tierScale = 0.65
)

// reducedResolution scales width/height down by tierScale and rounds to
// even (4:2:0 chroma subsampling needs even dimensions) — tier 1's
// resolution, computed live by changeTier from the camera's tier-0
// (configured) stream resolution.
func reducedResolution(width, height int) (int, int) {
	return evenize(int(float64(width) * tierScale)), evenize(int(float64(height) * tierScale))
}

func evenize(n int) int {
	if n < 2 {
		return 2
	}
	return n &^ 1 // clear the low bit
}

// videoStream is one camera's state as tracked by the coordinator.
type videoStream struct {
	name        string
	isMain      bool
	slot        *Slot
	gp          *GstPipeline
	encoderName string
	minBitrate  int
	maxBitrate  int

	// Tier-0 (configured) stream resolution/framerate, as actually running —
	// changeTier scales down from these, never from whatever the current
	// tier happens to be, so repeated switches can't compound rounding.
	baseWidth     int
	baseHeight    int
	baseFramerate int

	// Reactive bitrate state, mirrors what localStatsLoop used to keep locally.
	current      int
	stableCount  int
	prevSent     int64
	prevLost     int
	srtConnected bool
	paused       bool
	lastLossPct  float64
	lastRTTMS    float64

	// Resolution tier state.
	tier            int
	tierPinnedCount int
	tierStableCount int
}

// RegisterOptions are the parameters for BandwidthCoordinator.Register.
type RegisterOptions struct {
	Name        string
	IsMain      bool
	Slot        *Slot
	Pipeline    *GstPipeline
	EncoderName string
	MinBitrate  int
	MaxBitrate  int
	// Width, Height and Framerate are this camera's tier-0 (configured)
	// stream resolution, as actually running in Pipeline right now —
	// resolution-tier switching (changeTier) scales down from these via
	// Pipeline.SetResolution. Required for tier switching to function; a
	// zero Width/Height leaves it disabled for this camera (bitrate and
	// pause/resume are unaffected).
	Width, Height, Framerate int
}

// BandwidthCoordinator owns the shared video bandwidth budget for every
// currently-streaming camera. Construct with NewBandwidthCoordinator, run
// its allocation loop once with Run, and Register/Unregister cameras as
// their stream pipelines start and stop.
type BandwidthCoordinator struct {
	ctx context.Context

	mu      sync.Mutex
	streams map[string]*videoStream

	sharedBudget      int
	sharedStableCount int
	prevEffectiveMax  int
}

// NewBandwidthCoordinator returns an idle coordinator with no cameras
// registered yet. ctx is the same root context every other pipeline in the
// process uses — Run exits once it's done.
func NewBandwidthCoordinator(ctx context.Context) *BandwidthCoordinator {
	return &BandwidthCoordinator{ctx: ctx, streams: make(map[string]*videoStream)}
}

// Register adds a camera's running stream pipeline to the shared budget.
func (c *BandwidthCoordinator) Register(opts RegisterOptions) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.streams[opts.Name] = &videoStream{
		name:          opts.Name,
		isMain:        opts.IsMain,
		slot:          opts.Slot,
		gp:            opts.Pipeline,
		encoderName:   opts.EncoderName,
		minBitrate:    opts.MinBitrate,
		maxBitrate:    opts.MaxBitrate,
		baseWidth:     opts.Width,
		baseHeight:    opts.Height,
		baseFramerate: opts.Framerate,
	}
}

// Unregister removes a camera once its stream pipeline has fully stopped —
// call from the same onStopped hook activatePipeline/replacePipeline already
// provide (see pipeline.go), so a torn-down camera can't keep holding a
// share of the budget or skew the aggregate degradation signal with stale
// stats. gp must be the exact pipeline that stopped: onStopped runs
// asynchronously after teardown, so a fast stop/restart cycle for the same
// camera name could otherwise let a late Unregister for the OLD pipeline
// delete a NEW one already Register-ed in its place — gp identity, not just
// name, is what onStopped is actually reporting the end of.
func (c *BandwidthCoordinator) Unregister(name string, gp *GstPipeline) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if s, ok := c.streams[name]; ok && s.gp == gp {
		delete(c.streams, name)
	}
}

// Run starts the shared allocation loop, ticking at the same interval
// feedback.go's per-stream ABR used to. Blocks until the context passed to
// NewBandwidthCoordinator is done.
func (c *BandwidthCoordinator) Run() {
	ticker := time.NewTicker(fbLocalInterval)
	defer ticker.Stop()
	for {
		select {
		case <-c.ctx.Done():
			return
		case <-ticker.C:
			c.tick()
		}
	}
}

// tick gathers each registered camera's current SRT stats, adapts the
// shared budget from the worst case among them, re-allocates it, and
// evaluates whether any camera should switch resolution tier.
func (c *BandwidthCoordinator) tick() {
	c.mu.Lock()
	defer c.mu.Unlock()

	if len(c.streams) == 0 {
		return
	}

	totalMax := 0
	for _, s := range c.streams {
		totalMax += s.maxBitrate
	}
	if c.sharedBudget <= 0 {
		c.sharedBudget = totalMax // optimistic start, mirrors the old currentBitrate:=maxBitrate init
	}

	// Modem-tech ceiling on the aggregate — same tech/signal-based scaling
	// feedback.go always used, just applied to the whole pool instead of
	// each camera's own max independently.
	effectiveMax := totalMax
	if modemStats, ok := modem.CachedSignalStats(); ok {
		effectiveMax = modem.BitrateAdvisoryFromStats(totalMax, modemStats)
		if effectiveMax != c.prevEffectiveMax {
			logger.InfoFields("abr:shared",
				fmt.Sprintf("Modem ceiling (aggregate): %d bps (tech=%q signal=%d%%)", effectiveMax, modemStats.Tech, modemStats.Quality),
				map[string]any{"ceiling_bps": effectiveMax, "tech": modemStats.Tech, "signal_pct": modemStats.Quality})
			c.prevEffectiveMax = effectiveMax
		}
		if c.sharedBudget > effectiveMax {
			c.sharedBudget = effectiveMax
			c.sharedStableCount = 0
		}
	}

	var worstLoss, worstRTT float64
	anyConnected := false
	for _, s := range c.streams {
		if s.paused {
			continue // a paused stream's stats are stale/zero — would falsely look "clean"
		}
		rttMS, _, sentTotal, lostTotal := s.gp.GetSRTSinkStats("srtsink")
		if sentTotal == 0 {
			continue // not connected yet
		}
		if !s.srtConnected {
			s.srtConnected = true
			logger.Info("[abr:%s] SRT connected — forcing IDR", s.name)
			s.gp.ForceIDR(s.encoderName)
		}
		anyConnected = true

		deltaSent := sentTotal - s.prevSent
		deltaLost := lostTotal - s.prevLost
		s.prevSent, s.prevLost = sentTotal, lostTotal

		var lossPct float64
		if total := float64(deltaSent) + float64(deltaLost); total > 0 {
			lossPct = 100.0 * float64(deltaLost) / total
		}
		s.lastLossPct, s.lastRTTMS = lossPct, rttMS
		if lossPct > worstLoss {
			worstLoss = lossPct
		}
		if rttMS > worstRTT {
			worstRTT = rttMS
		}
	}
	minSharedFloor := effectiveMax / 5
	switch {
	case anyConnected:
		c.sharedBudget = adaptBitrate(c.sharedBudget, localStats{LossPct: worstLoss, RTTMS: worstRTT}, minSharedFloor, effectiveMax, &c.sharedStableCount)
	case !c.anyEverConnected():
		// Nothing has ever connected yet (e.g. right after startup, before
		// any camera's first SRT handshake) — there are no stats to adapt
		// the shared budget from, and nothing to pause/resume yet either.
		return
	default:
		// Everything is currently paused/disconnected, but something
		// connected before: there's no live stats to adapt from, but
		// freezing sharedBudget here forever would leave it stuck below
		// every camera's floor if it collapsed there right as the last
		// connection dropped — nothing could ever prove conditions
		// improved, since improving requires resuming a camera, and
		// resuming requires the budget to clear the floor first. Ramp it
		// back toward effectiveMax on assumed-perfect stats instead — the
		// same step adaptBitrate already takes on real good stats — so
		// it's always possible to climb back out and let allocate() try
		// resuming a camera to re-test real conditions.
		c.sharedBudget = adaptBitrate(c.sharedBudget, localStats{}, minSharedFloor, effectiveMax, &c.sharedStableCount)
	}
	// Note: even when every stream is currently paused (anyConnected false
	// but something connected before), allocate/evaluateTiers must still run
	// — pause()/resume() are only ever called from allocate(), so skipping
	// it here would permanently strand every camera in the paused state
	// once the shared budget ever collapsed below all their floors, with no
	// path back to streaming short of a process restart.

	c.allocate()
	c.evaluateTiers()
}

// anyEverConnected reports whether any registered stream has completed at
// least one SRT handshake so far (srtConnected latches true and is never
// reset by pause — see the tick loop above and pause()).
func (c *BandwidthCoordinator) anyEverConnected() bool {
	for _, s := range c.streams {
		if s.srtConnected {
			return true
		}
	}
	return false
}

// streamSpec is the pure-data view of a videoStream computeAllocation needs —
// kept separate from videoStream itself (which holds live *GstPipeline/*Slot
// pointers) so the allocation policy is testable without any GStreamer
// dependency.
type streamSpec struct {
	name       string
	isMain     bool
	minBitrate int
	maxBitrate int
}

// allocation is one camera's outcome for this tick: either paused, or
// streaming at ceiling bps (its new ceiling — actual bitrate still comes
// from adaptBitrate reacting to that camera's own recent loss/RTT within it).
type allocation struct {
	paused  bool
	ceiling int
}

// computeAllocation is the pure allocation policy: the main camera (if any)
// gets up to its own configured max first and is never paused — it floors
// out at its minBitrate instead, same as the old per-stream behavior, if the
// budget can't even cover that. Secondary cameras split what's left, in
// name order (so allocation isn't sensitive to Go's random map iteration
// order between ticks), and are paused outright once the remaining budget
// can't cover their own floor — no point running a secondary camera at a
// bitrate too low to be worth decoding when the alternative is one fewer
// stream contending for the same shared link.
func computeAllocation(budget int, streams []streamSpec) map[string]allocation {
	result := make(map[string]allocation, len(streams))

	var mains, secondaries []streamSpec
	for _, s := range streams {
		if s.isMain {
			mains = append(mains, s)
		} else {
			secondaries = append(secondaries, s)
		}
	}
	sort.Slice(mains, func(i, j int) bool { return mains[i].name < mains[j].name })
	sort.Slice(secondaries, func(i, j int) bool { return secondaries[i].name < secondaries[j].name })

	for _, s := range mains {
		ceiling := s.maxBitrate
		if ceiling > budget {
			ceiling = budget
		}
		if ceiling < s.minBitrate {
			ceiling = s.minBitrate
		}
		result[s.name] = allocation{ceiling: ceiling}
		budget -= ceiling
		if budget < 0 {
			budget = 0
		}
	}

	for _, s := range secondaries {
		if budget < s.minBitrate {
			result[s.name] = allocation{paused: true}
			continue
		}
		ceiling := s.maxBitrate
		if ceiling > budget {
			ceiling = budget
		}
		result[s.name] = allocation{ceiling: ceiling}
		budget -= ceiling
	}

	return result
}

// allocate applies computeAllocation's decision for this tick: pausing or
// resuming secondary cameras as needed, and reactively fine-tuning each
// unpaused camera's actual bitrate within its new ceiling using that
// camera's own recent loss/RTT (adaptBitrate — same function and same
// per-camera stats feedback.go's old per-stream loop used, just fed a
// coordinated ceiling instead of an independently-computed one).
func (c *BandwidthCoordinator) allocate() {
	specs := make([]streamSpec, 0, len(c.streams))
	for _, s := range c.streams {
		specs = append(specs, streamSpec{name: s.name, isMain: s.isMain, minBitrate: s.minBitrate, maxBitrate: s.maxBitrate})
	}
	plan := computeAllocation(c.sharedBudget, specs)

	for _, s := range c.streams {
		a := plan[s.name]
		if a.paused {
			c.pause(s)
			continue
		}
		c.resume(s)
		c.applyCeiling(s, a.ceiling)
	}
}

func (c *BandwidthCoordinator) applyCeiling(s *videoStream, ceiling int) {
	if s.current == 0 {
		s.current = ceiling // first allocation: start at the ceiling, mirrors the old init-at-max
	}
	newBitrate := adaptBitrate(s.current, localStats{LossPct: s.lastLossPct, RTTMS: s.lastRTTMS}, s.minBitrate, ceiling, &s.stableCount)
	if newBitrate > ceiling {
		// The ceiling itself just dropped below where this camera's own
		// reactive state had it — clamp immediately rather than waiting for
		// adaptBitrate's own degraded-sample path to notice on some future
		// tick.
		newBitrate = ceiling
		s.stableCount = 0
	}
	if newBitrate != s.current {
		logger.InfoFields("abr:"+s.name,
			fmt.Sprintf("Bitrate %d → %d bps (ceiling=%d loss=%.1f%% rtt=%.0fms)", s.current, newBitrate, ceiling, s.lastLossPct, s.lastRTTMS),
			map[string]any{"from_bps": s.current, "to_bps": newBitrate, "ceiling_bps": ceiling, "loss_pct": s.lastLossPct, "rtt_ms": s.lastRTTMS})
		s.gp.SetBitrate(s.encoderName, newBitrate)
		s.current = newBitrate
	}
}

func (c *BandwidthCoordinator) pause(s *videoStream) {
	if s.paused {
		return
	}
	s.paused = true
	s.stableCount = 0
	s.tierPinnedCount, s.tierStableCount = 0, 0
	logger.InfoFields("abr:"+s.name,
		fmt.Sprintf("[%s] Paused — shared bandwidth budget can't cover even its floor (%d bps)", s.name, s.minBitrate),
		map[string]any{"camera": s.name, "reason": "bandwidth_pause", "floor_bps": s.minBitrate})
	s.slot.PauseStream()
}

func (c *BandwidthCoordinator) resume(s *videoStream) {
	if !s.paused {
		return
	}
	s.paused = false
	logger.Info("[abr:%s] Resumed — shared bandwidth budget has room again", s.name)
	s.slot.ResumeStream() // also forces an IDR — see its own doc comment
}

// tierDecision is decideTier's pure output: the pinned/stable counts to
// store for next tick, and the tier to switch to (equal to the input tier
// when no switch is warranted yet).
type tierDecision struct {
	newTier         int
	tierPinnedCount int
	tierStableCount int
}

// decideTier is the resolution-tier policy: given a camera's current tier,
// its hysteresis counters, and this tick's reactive bitrate/loss/RTT state,
// decide whether to switch. Kept separate from evaluateTiers (which also has
// to call the real pipeline rebuild) so the threshold logic is unit-testable
// without any GStreamer dependency. See tierDowngradeTicks/tierUpgradeTicks
// for why this reacts far more slowly than the bitrate ABR itself: a tier
// switch is a visible ~50ms+ glitch (measured — see CLAUDE.md), not just a
// silent bitrate number changing, and must not fire on a single bad or good
// sample. Downgrade fires when a camera has been pinned at its own bitrate
// floor *and* still degraded for tierDowngradeTicks ticks in a row —
// bitrate alone isn't enough any more. Upgrade fires after tierUpgradeTicks
// of clean stats at a reduced tier.
func decideTier(tier, pinnedCount, stableCount int, atFloor, degraded, stable bool) tierDecision {
	if tier == 0 {
		newPinned := 0
		if atFloor && degraded {
			newPinned = pinnedCount + 1
		}
		newTier := 0
		if newPinned >= tierDowngradeTicks {
			newTier = 1
			newPinned = 0
		}
		return tierDecision{newTier: newTier, tierPinnedCount: newPinned}
	}

	newStable := 0
	if stable {
		newStable = stableCount + 1
	}
	newTier := tier
	if newStable >= tierUpgradeTicks {
		newTier = 0
		newStable = 0
	}
	return tierDecision{newTier: newTier, tierStableCount: newStable}
}

// evaluateTiers runs decideTier for every unpaused, tier-capable camera and
// acts on the result.
func (c *BandwidthCoordinator) evaluateTiers() {
	for _, s := range c.streams {
		if s.paused || s.baseWidth == 0 || s.baseHeight == 0 {
			s.tierPinnedCount, s.tierStableCount = 0, 0
			continue
		}
		degraded := s.lastLossPct > fbDecreaseLoss || s.lastRTTMS > fbDecreaseRTT
		stable := s.lastLossPct < fbIncreaseLoss && s.lastRTTMS < fbIncreaseRTT
		atFloor := s.current <= s.minBitrate

		d := decideTier(s.tier, s.tierPinnedCount, s.tierStableCount, atFloor, degraded, stable)
		s.tierPinnedCount, s.tierStableCount = d.tierPinnedCount, d.tierStableCount
		if d.newTier != s.tier {
			c.changeTier(s, d.newTier)
		}
	}
}

// changeTier switches s's encoder to newTier's resolution live, via
// GstPipeline.SetResolution on the running pipeline's capsfilter — no
// pipeline rebuild, no dropped/re-established SRT connection. Confirmed on
// real hardware (the Jetson AV1 encoder's own DRC support): a ~60ms
// glitch-free transition, srtsink and its cumulative stats untouched (see
// CLAUDE.md for the measurement). The bitrate the encoder was already
// targeting is unaffected by a resolution change, so none of this camera's
// reactive bitrate state needs to reset either — only the tier itself and
// its hysteresis counters change.
func (c *BandwidthCoordinator) changeTier(s *videoStream, newTier int) {
	direction := "recovery"
	if newTier > s.tier {
		direction = "sustained pressure"
	}
	width, height := s.baseWidth, s.baseHeight
	if newTier > 0 {
		width, height = reducedResolution(width, height)
	}
	logger.InfoFields("abr:"+s.name,
		fmt.Sprintf("[%s] Switching resolution tier %d → %d (%s): %dx%d", s.name, s.tier, newTier, direction, width, height),
		map[string]any{"camera": s.name, "from_tier": s.tier, "to_tier": newTier, "reason": direction, "width": width, "height": height})

	s.gp.SetResolution(resolutionCapsfilterName, width, height, s.baseFramerate)
	s.tier = newTier
	s.tierPinnedCount, s.tierStableCount = 0, 0
}
