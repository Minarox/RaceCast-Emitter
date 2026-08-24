package pipeline

import "testing"

func TestAdaptBitrate_DegradedLossDecreasesAndResetsStableCount(t *testing.T) {
	const min, max = 1_000_000, 10_000_000
	stable := 5
	got := adaptBitrate(5_000_000, localStats{LossPct: 5.0, RTTMS: 50}, min, max, &stable)
	want := int(5_000_000 * fbDecreaseFactor)
	if got != want {
		t.Errorf("adaptBitrate() = %d, want %d", got, want)
	}
	if stable != 0 {
		t.Errorf("stableCount = %d, want reset to 0", stable)
	}
}

func TestAdaptBitrate_DegradedRTTDecreases(t *testing.T) {
	const min, max = 1_000_000, 10_000_000
	stable := 0
	got := adaptBitrate(5_000_000, localStats{LossPct: 0, RTTMS: 200}, min, max, &stable)
	want := int(5_000_000 * fbDecreaseFactor)
	if got != want {
		t.Errorf("adaptBitrate() = %d, want %d", got, want)
	}
}

func TestAdaptBitrate_DecreaseClampedToMin(t *testing.T) {
	const min, max = 1_000_000, 10_000_000
	stable := 0
	got := adaptBitrate(1_100_000, localStats{LossPct: 10, RTTMS: 300}, min, max, &stable)
	if got != min {
		t.Errorf("adaptBitrate() = %d, want clamped to min %d", got, min)
	}
}

func TestAdaptBitrate_StableSampleBelowThresholdHoldsAndIncrementsCount(t *testing.T) {
	const min, max = 1_000_000, 10_000_000
	stable := 0
	got := adaptBitrate(5_000_000, localStats{LossPct: 0.1, RTTMS: 50}, min, max, &stable)
	if got != 5_000_000 {
		t.Errorf("adaptBitrate() = %d, want unchanged 5000000 before %d stable samples", got, fbStableNeeded)
	}
	if stable != 1 {
		t.Errorf("stableCount = %d, want 1", stable)
	}
}

func TestAdaptBitrate_IncreasesAfterEnoughStableSamples(t *testing.T) {
	const min, max = 1_000_000, 10_000_000
	stable := fbStableNeeded - 1
	got := adaptBitrate(5_000_000, localStats{LossPct: 0.1, RTTMS: 50}, min, max, &stable)
	want := int(5_000_000 * fbIncreaseFactor)
	if got != want {
		t.Errorf("adaptBitrate() = %d, want %d", got, want)
	}
	if stable != 0 {
		t.Errorf("stableCount = %d, want reset to 0 after increase", stable)
	}
}

func TestAdaptBitrate_IncreaseClampedToMax(t *testing.T) {
	const min, max = 1_000_000, 10_000_000
	stable := fbStableNeeded - 1
	got := adaptBitrate(9_800_000, localStats{LossPct: 0, RTTMS: 10}, min, max, &stable)
	if got != max {
		t.Errorf("adaptBitrate() = %d, want clamped to max %d", got, max)
	}
}

func TestAdaptBitrate_AlreadyAtMaxDoesNotAccumulateStableCount(t *testing.T) {
	const min, max = 1_000_000, 10_000_000
	stable := 0
	got := adaptBitrate(max, localStats{LossPct: 0, RTTMS: 10}, min, max, &stable)
	if got != max {
		t.Errorf("adaptBitrate() = %d, want %d", got, max)
	}
	if stable != 0 {
		t.Errorf("stableCount = %d, want 0 (current already at max)", stable)
	}
}

func TestAdaptBitrate_MidRangeSampleHoldsAndDoesNotTouchStableCount(t *testing.T) {
	const min, max = 1_000_000, 10_000_000
	stable := 2
	got := adaptBitrate(5_000_000, localStats{LossPct: 2.0, RTTMS: 100}, min, max, &stable)
	if got != 5_000_000 {
		t.Errorf("adaptBitrate() = %d, want unchanged 5000000", got)
	}
	if stable != 2 {
		t.Errorf("stableCount = %d, want unchanged 2 (sample neither degraded nor stable)", stable)
	}
}
