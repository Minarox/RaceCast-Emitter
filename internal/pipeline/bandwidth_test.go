package pipeline

import (
	"context"
	"testing"
)

// TestBandwidthCoordinator_UnregisterOnlyRemovesMatchingPipeline reproduces
// a fast stop/restart cycle for the same camera name: onStopped (which
// calls Unregister) runs asynchronously after a pipeline's teardown
// completes, so a new pipeline can already be Register-ed under the same
// name by the time the OLD pipeline's Unregister call actually runs. It
// must not delete the newer registration.
func TestBandwidthCoordinator_UnregisterOnlyRemovesMatchingPipeline(t *testing.T) {
	c := NewBandwidthCoordinator(context.Background())

	oldGP := &GstPipeline{}
	newGP := &GstPipeline{}

	c.Register(RegisterOptions{Name: "Cockpit", Pipeline: oldGP, MinBitrate: 1_000_000, MaxBitrate: 8_000_000})
	c.Register(RegisterOptions{Name: "Cockpit", Pipeline: newGP, MinBitrate: 1_000_000, MaxBitrate: 8_000_000})

	c.Unregister("Cockpit", oldGP)

	c.mu.Lock()
	got, ok := c.streams["Cockpit"]
	c.mu.Unlock()
	if !ok {
		t.Fatal(`Unregister("Cockpit", oldGP) removed the newer registration — want it left untouched`)
	}
	if got.gp != newGP {
		t.Errorf("streams[Cockpit].gp = %p, want the newer pipeline %p", got.gp, newGP)
	}

	// Unregistering with the currently-registered pipeline must still work.
	c.Unregister("Cockpit", newGP)
	c.mu.Lock()
	_, ok = c.streams["Cockpit"]
	c.mu.Unlock()
	if ok {
		t.Error(`Unregister("Cockpit", newGP) did not remove the matching registration`)
	}
}

func TestComputeAllocation_MainCameraGetsFullShareFirst(t *testing.T) {
	streams := []streamSpec{
		{name: "Front", isMain: true, minBitrate: 1_000_000, maxBitrate: 8_000_000},
		{name: "Rear", isMain: false, minBitrate: 1_000_000, maxBitrate: 8_000_000},
	}
	// Plenty of budget for both.
	plan := computeAllocation(20_000_000, streams)

	if plan["Front"].paused {
		t.Error("main camera must never be paused")
	}
	if plan["Front"].ceiling != 8_000_000 {
		t.Errorf("Front ceiling = %d, want 8_000_000 (its own max)", plan["Front"].ceiling)
	}
	if plan["Rear"].paused {
		t.Error("Rear should not be paused when budget covers it")
	}
	if plan["Rear"].ceiling != 8_000_000 {
		t.Errorf("Rear ceiling = %d, want 8_000_000", plan["Rear"].ceiling)
	}
}

func TestComputeAllocation_SecondaryPausedWhenBudgetInsufficient(t *testing.T) {
	streams := []streamSpec{
		{name: "Front", isMain: true, minBitrate: 1_000_000, maxBitrate: 8_000_000},
		{name: "Rear", isMain: false, minBitrate: 1_000_000, maxBitrate: 8_000_000},
	}
	// Only enough for the main camera plus a sliver — not enough for Rear's floor.
	plan := computeAllocation(8_500_000, streams)

	if plan["Front"].paused {
		t.Error("main camera must never be paused")
	}
	if plan["Front"].ceiling != 8_000_000 {
		t.Errorf("Front ceiling = %d, want 8_000_000", plan["Front"].ceiling)
	}
	if !plan["Rear"].paused {
		t.Errorf("Rear should be paused: only %d bps left, below its %d bps floor", 500_000, streams[1].minBitrate)
	}
}

func TestComputeAllocation_MainCameraFloorsOutRatherThanPaused(t *testing.T) {
	streams := []streamSpec{
		{name: "Front", isMain: true, minBitrate: 1_000_000, maxBitrate: 8_000_000},
	}
	// Budget far below even the main camera's floor.
	plan := computeAllocation(100_000, streams)

	if plan["Front"].paused {
		t.Error("main camera must never be paused, even under a starved budget")
	}
	if plan["Front"].ceiling != streams[0].minBitrate {
		t.Errorf("Front ceiling = %d, want its floor %d when budget is below it", plan["Front"].ceiling, streams[0].minBitrate)
	}
}

func TestComputeAllocation_SecondariesSplitRemainderAfterMain(t *testing.T) {
	streams := []streamSpec{
		{name: "Front", isMain: true, minBitrate: 1_000_000, maxBitrate: 6_000_000},
		{name: "Cockpit", isMain: false, minBitrate: 500_000, maxBitrate: 4_000_000},
		{name: "Rear", isMain: false, minBitrate: 500_000, maxBitrate: 4_000_000},
	}
	// 6M (Front's full max) + 3M remaining for the two secondaries to split
	// in name order: Cockpit first (up to its own max, 3M available < 4M max
	// -> gets all 3M), leaving 0 for Rear.
	plan := computeAllocation(9_000_000, streams)

	if plan["Front"].ceiling != 6_000_000 {
		t.Errorf("Front ceiling = %d, want 6_000_000", plan["Front"].ceiling)
	}
	if plan["Cockpit"].paused || plan["Cockpit"].ceiling != 3_000_000 {
		t.Errorf("Cockpit = %+v, want unpaused with ceiling 3_000_000", plan["Cockpit"])
	}
	if !plan["Rear"].paused {
		t.Errorf("Rear should be paused: nothing left after Front and Cockpit, got %+v", plan["Rear"])
	}
}

func TestComputeAllocation_NoMainCameraTreatsAllAsSecondary(t *testing.T) {
	streams := []streamSpec{
		{name: "A", isMain: false, minBitrate: 1_000_000, maxBitrate: 5_000_000},
		{name: "B", isMain: false, minBitrate: 1_000_000, maxBitrate: 5_000_000},
	}
	// Enough for A (name order) but nothing left for B.
	plan := computeAllocation(5_000_000, streams)

	if plan["A"].paused || plan["A"].ceiling != 5_000_000 {
		t.Errorf("A = %+v, want unpaused with ceiling 5_000_000", plan["A"])
	}
	if !plan["B"].paused {
		t.Errorf("B should be paused with no budget left, got %+v", plan["B"])
	}
}

func TestComputeAllocation_ZeroCamerasIsSafe(t *testing.T) {
	plan := computeAllocation(1_000_000, nil)
	if len(plan) != 0 {
		t.Errorf("expected an empty plan, got %v", plan)
	}
}

func TestDecideTier_DowngradesOnlyAfterSustainedPinnedAndDegraded(t *testing.T) {
	pinned := 0
	for i := 0; i < tierDowngradeTicks-1; i++ {
		d := decideTier(0, pinned, 0, true /* atFloor */, true /* degraded */, false)
		if d.newTier != 0 {
			t.Fatalf("tick %d: switched too early (tier=%d), want still 0", i, d.newTier)
		}
		pinned = d.tierPinnedCount
	}
	d := decideTier(0, pinned, 0, true, true, false)
	if d.newTier != 1 {
		t.Errorf("after %d sustained ticks, newTier = %d, want 1", tierDowngradeTicks, d.newTier)
	}
	if d.tierPinnedCount != 0 {
		t.Errorf("tierPinnedCount after switching = %d, want reset to 0", d.tierPinnedCount)
	}
}

func TestDecideTier_PinnedCountResetsOnAnyGoodSample(t *testing.T) {
	// One tick short of triggering a downgrade.
	d := decideTier(0, tierDowngradeTicks-2, 0, true, true, false)
	if d.newTier != 0 {
		t.Fatalf("should not have switched yet, got tier %d", d.newTier)
	}
	if d.tierPinnedCount != tierDowngradeTicks-1 {
		t.Fatalf("tierPinnedCount = %d, want %d", d.tierPinnedCount, tierDowngradeTicks-1)
	}
	// Not degraded this tick — even though still at the floor, that alone
	// must not count toward a downgrade.
	d = decideTier(0, d.tierPinnedCount, 0, true, false, true)
	if d.tierPinnedCount != 0 {
		t.Errorf("tierPinnedCount = %d after a clean sample, want reset to 0", d.tierPinnedCount)
	}
	if d.newTier != 0 {
		t.Errorf("newTier = %d, want still 0", d.newTier)
	}
}

func TestDecideTier_NotPinnedAtFloorNeverAccumulates(t *testing.T) {
	// Degraded, but not at the bitrate floor: bitrate ABR can still handle
	// it on its own, no reason to consider a resolution downgrade yet.
	d := decideTier(0, 3, 0, false /* atFloor */, true /* degraded */, false)
	if d.tierPinnedCount != 0 {
		t.Errorf("tierPinnedCount = %d, want 0 when not at floor", d.tierPinnedCount)
	}
}

func TestDecideTier_UpgradesOnlyAfterSustainedStable(t *testing.T) {
	stable := 0
	for i := 0; i < tierUpgradeTicks-1; i++ {
		d := decideTier(1, 0, stable, false, false, true /* stable */)
		if d.newTier != 1 {
			t.Fatalf("tick %d: switched too early (tier=%d), want still 1", i, d.newTier)
		}
		stable = d.tierStableCount
	}
	d := decideTier(1, 0, stable, false, false, true)
	if d.newTier != 0 {
		t.Errorf("after %d sustained clean ticks, newTier = %d, want 0", tierUpgradeTicks, d.newTier)
	}
	if d.tierStableCount != 0 {
		t.Errorf("tierStableCount after switching = %d, want reset to 0", d.tierStableCount)
	}
}

func TestDecideTier_StableCountResetsOnAnyBadSample(t *testing.T) {
	// One tick short of triggering an upgrade.
	d := decideTier(1, 0, tierUpgradeTicks-2, false, false, true)
	if d.newTier != 1 {
		t.Fatalf("should not have switched yet, got tier %d", d.newTier)
	}
	if d.tierStableCount != tierUpgradeTicks-1 {
		t.Fatalf("tierStableCount = %d, want %d", d.tierStableCount, tierUpgradeTicks-1)
	}
	d = decideTier(1, 0, d.tierStableCount, false, false, false /* not stable */)
	if d.tierStableCount != 0 {
		t.Errorf("tierStableCount = %d after a non-stable sample, want reset to 0", d.tierStableCount)
	}
}

func TestEvenize(t *testing.T) {
	tests := []struct{ in, want int }{
		{0, 2}, {1, 2}, {2, 2}, {3, 2}, {4, 4}, {5, 4}, {720, 720}, {721, 720},
	}
	for _, tt := range tests {
		if got := evenize(tt.in); got != tt.want {
			t.Errorf("evenize(%d) = %d, want %d", tt.in, got, tt.want)
		}
	}
}

func TestReducedResolution_ScalesAndKeepsEven(t *testing.T) {
	w, h := reducedResolution(960, 540)

	if w%2 != 0 || h%2 != 0 {
		t.Errorf("reduced dimensions must be even, got %dx%d", w, h)
	}
	if w >= 960 || h >= 540 {
		t.Errorf("reduced dimensions %dx%d not smaller than original 960x540", w, h)
	}
}
