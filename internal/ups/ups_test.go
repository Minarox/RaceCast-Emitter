package ups

import "testing"

// TestDecode covers the INA219 register-decode arithmetic (bus-voltage >>3
// shift, current/power's signed int16 handling, the percentage clamp) —
// previously exercised only by the real I2C-backed Read(), so a sign or
// shift error here would have shipped unnoticed.
func TestDecode(t *testing.T) {
	tests := []struct {
		name                string
		vRaw, aRaw, wRaw    uint16
		wantV, wantA, wantW float64
		wantP               float64
	}{
		{
			name: "full battery, positive current/power",
			vRaw: 24000, aRaw: 10000, wRaw: 5000,
			wantV: 12, wantA: 1.52, wantW: 15.24, wantP: 83.33,
		},
		{
			// aRaw/wRaw = uint16(int16(-1000)): current/power registers are
			// signed (negative = charging) — a naive unsigned read would
			// produce a large positive value instead.
			name: "negative current/power (charging) decoded as negative, not a huge positive",
			vRaw: 24000, aRaw: 64536, wRaw: 64536,
			wantV: 12, wantA: -0.15, wantW: -3.05, wantP: 83.33,
		},
		{
			name: "percentage clamps to 0 below batteryMin, not negative",
			vRaw: 1000, aRaw: 0, wRaw: 0,
			wantV: 0.5, wantA: 0, wantW: 0, wantP: 0,
		},
		{
			name: "percentage clamps to 100 above batteryMax, not over 100",
			vRaw: 30000, aRaw: 0, wRaw: 0,
			wantV: 15, wantA: 0, wantW: 0, wantP: 100,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := decode(tt.vRaw, tt.aRaw, tt.wRaw)
			if got.Voltage != tt.wantV {
				t.Errorf("Voltage = %v, want %v", got.Voltage, tt.wantV)
			}
			if got.Current != tt.wantA {
				t.Errorf("Current = %v, want %v", got.Current, tt.wantA)
			}
			if got.Power != tt.wantW {
				t.Errorf("Power = %v, want %v", got.Power, tt.wantW)
			}
			if got.Percentage != tt.wantP {
				t.Errorf("Percentage = %v, want %v", got.Percentage, tt.wantP)
			}
		})
	}
}

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
