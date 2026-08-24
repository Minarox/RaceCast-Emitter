package ups

import "testing"

func TestRound2(t *testing.T) {
	tests := []struct {
		in, want float64
	}{
		{12.3456, 12.35},
		{12.344, 12.34},
		{0, 0},
		{-3.456, -3.46},
	}
	for _, tt := range tests {
		if got := round2(tt.in); got != tt.want {
			t.Errorf("round2(%v) = %v, want %v", tt.in, got, tt.want)
		}
	}
}
