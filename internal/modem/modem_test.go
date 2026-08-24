package modem

import (
	"math"
	"testing"
)

func TestValidNMEA(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want bool
	}{
		{"valid GGA checksum", "$GPGGA,123519,4807.038,N,01131.000,E,1,08,0.9,545.4,M,46.9,M,,*47", true},
		{"valid RMC checksum", "$GPRMC,123519,A,4807.038,N,01131.000,E,022.4,084.4,230394,003.1,W*6A", true},
		{"mismatched checksum", "$GPGGA,123519,4807.038,N,01131.000,E,1,08,0.9,545.4,M,46.9,M,,*00", false},
		{"no checksum field", "$GPGGA,123519,4807.038,N,01131.000,E,1,08,0.9,545.4,M,46.9,M,,", true},
		{"non-hex checksum digits", "$GPGGA,foo*ZZ", false},
		{"too short after star to hold a checksum", "$GPGGA,foo*4", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := validNMEA(tt.in); got != tt.want {
				t.Errorf("validNMEA(%q) = %v, want %v", tt.in, got, tt.want)
			}
		})
	}
}

func TestParseNMEA_GGAFix(t *testing.T) {
	sentences := []string{
		"$GPGGA,123519,4807.038,N,01131.000,E,1,08,0.9,545.4,M,46.9,M,,*47",
	}
	pos, ok := ParseNMEA(sentences)
	if !ok {
		t.Fatal("ParseNMEA() ok = false, want true")
	}
	if !pos.Fix {
		t.Error("pos.Fix = false, want true")
	}
	if math.Abs(pos.Lat-48.1173) > 1e-3 {
		t.Errorf("pos.Lat = %v, want ~48.1173", pos.Lat)
	}
	if math.Abs(pos.Lon-11.51667) > 1e-3 {
		t.Errorf("pos.Lon = %v, want ~11.51667", pos.Lon)
	}
	if pos.Sats != 8 {
		t.Errorf("pos.Sats = %d, want 8", pos.Sats)
	}
	if pos.HDOP != 0.9 {
		t.Errorf("pos.HDOP = %v, want 0.9", pos.HDOP)
	}
	if pos.Alt != 545.4 {
		t.Errorf("pos.Alt = %v, want 545.4", pos.Alt)
	}
}

func TestParseNMEA_GNPrefixAccepted(t *testing.T) {
	sentences := []string{
		"$GNGGA,123519,4807.038,N,01131.000,E,1,08,0.9,545.4,M,46.9,M,,*45",
	}
	pos, ok := ParseNMEA(sentences)
	if !ok || !pos.Fix {
		t.Fatalf("expected a fix from $GNGGA, got pos=%+v ok=%v", pos, ok)
	}
}

func TestParseNMEA_NoFixStillReportsSatsAndHDOP(t *testing.T) {
	sentences := []string{
		"$GPGGA,123519,,,,,0,03,2.5,,,,,,*66",
	}
	pos, ok := ParseNMEA(sentences)
	if !ok {
		t.Fatal("ParseNMEA() ok = false, want true (frame present even without fix)")
	}
	if pos.Fix {
		t.Error("pos.Fix = true, want false (quality=0)")
	}
	if pos.Sats != 3 {
		t.Errorf("pos.Sats = %d, want 3", pos.Sats)
	}
	if pos.HDOP != 2.5 {
		t.Errorf("pos.HDOP = %v, want 2.5", pos.HDOP)
	}
}

func TestParseNMEA_RMCActiveAndVoid(t *testing.T) {
	active := []string{
		"$GPRMC,123519,A,4807.038,N,01131.000,E,022.4,084.4,230394,003.1,W*6A",
	}
	pos, ok := ParseNMEA(active)
	if !ok {
		t.Fatal("ParseNMEA() ok = false for active RMC, want true")
	}
	if pos.Speed != 22.4 {
		t.Errorf("pos.Speed = %v, want 22.4", pos.Speed)
	}
	if pos.Course != 84.4 {
		t.Errorf("pos.Course = %v, want 84.4", pos.Course)
	}

	void := []string{
		"$GPRMC,123519,V,,,,,,,230394,,*3F",
	}
	if _, ok := ParseNMEA(void); ok {
		t.Error("ParseNMEA() ok = true for void (V) RMC, want false")
	}
}

func TestParseNMEA_CombinedGGARMC(t *testing.T) {
	sentences := []string{
		"$GPGGA,123519,4807.038,N,01131.000,E,1,08,0.9,545.4,M,46.9,M,,*47",
		"$GPRMC,123519,A,4807.038,N,01131.000,E,022.4,084.4,230394,003.1,W*6A",
	}
	pos, ok := ParseNMEA(sentences)
	if !ok || !pos.Fix {
		t.Fatalf("expected combined fix, got pos=%+v ok=%v", pos, ok)
	}
	if pos.Speed != 22.4 || pos.Alt != 545.4 {
		t.Errorf("combined parse missing fields: %+v", pos)
	}
}

func TestParseNMEA_NoUsableSentence(t *testing.T) {
	if _, ok := ParseNMEA(nil); ok {
		t.Error("ParseNMEA(nil) ok = true, want false")
	}
	if _, ok := ParseNMEA([]string{"$GPGSV,3,1,10*7A"}); ok {
		t.Error("ParseNMEA() ok = true for an unrelated sentence type, want false")
	}
}

func TestParseGGA_TooFewFields(t *testing.T) {
	if _, ok := parseGGA("$GPGGA,1,2,3"); ok {
		t.Error("parseGGA() ok = true for a truncated sentence, want false")
	}
}

func TestParseRMC_TooFewFields(t *testing.T) {
	if _, ok := parseRMC("$GPRMC,1,A,2"); ok {
		t.Error("parseRMC() ok = true for a truncated sentence, want false")
	}
}

func TestNmeaToDecimal(t *testing.T) {
	tests := []struct {
		name     string
		raw      string
		hemi     string
		wantOK   bool
		wantDec  float64
		wantSign int // used only when wantOK
	}{
		{"north", "4807.038", "N", true, 48.1173, 1},
		{"south negates", "4807.038", "S", true, 48.1173, -1},
		{"east", "01131.000", "E", true, 11.51667, 1},
		{"west negates", "01131.000", "W", true, 11.51667, -1},
		{"empty raw", "", "N", false, 0, 0},
		{"missing dot", "4807", "N", false, 0, 0},
		{"dot too early", "4.038", "N", false, 0, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := nmeaToDecimal(tt.raw, tt.hemi)
			if ok != tt.wantOK {
				t.Fatalf("nmeaToDecimal(%q,%q) ok = %v, want %v", tt.raw, tt.hemi, ok, tt.wantOK)
			}
			if !ok {
				return
			}
			want := tt.wantDec * float64(tt.wantSign)
			if math.Abs(got-want) > 1e-3 {
				t.Errorf("nmeaToDecimal(%q,%q) = %v, want ~%v", tt.raw, tt.hemi, got, want)
			}
		})
	}
}

func TestBitrateAdvisoryFromStats(t *testing.T) {
	const max = 10_000_000

	tests := []struct {
		name    string
		tech    string
		quality uint32
		want    int
	}{
		{"LTE full signal: no penalty", "lte", 80, max},
		{"5G NR full signal: no penalty", "5gnr", 90, max},
		{"LTE uppercase tech is case-insensitive", "LTE", 80, max},
		{"HSPA+ full signal: 85%", "hspa+", 80, int(float64(max) * 0.85)},
		{"HSDPA full signal: 85%", "hsdpa", 80, int(float64(max) * 0.85)},
		{"UMTS full signal: 55%", "umts", 80, int(float64(max) * 0.55)},
		{"UMTS medium signal: 55% * 85%", "umts", 40, int(float64(max) * 0.55 * 0.85)},
		{"GSM full signal floored at 20%, above ABR floor", "gsm", 80, int(float64(max) * 0.20)},
		{"unknown tech (empty) with no signal falls back to signal-only penalty", "", 0, int(float64(max) * 1.0 * 0.60)},
		{"GSM with poor signal clamped to the ABR floor (max/5)", "gsm", 10, max / 5},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := BitrateAdvisoryFromStats(max, SignalStats{Tech: tt.tech, Quality: tt.quality})
			if got != tt.want {
				t.Errorf("BitrateAdvisoryFromStats(%d, {%q,%d}) = %d, want %d", max, tt.tech, tt.quality, got, tt.want)
			}
		})
	}
}

func TestBitrateAdvisoryFromStats_NeverBelowFloor(t *testing.T) {
	const max = 1_000_000
	got := BitrateAdvisoryFromStats(max, SignalStats{Tech: "gprs", Quality: 5})
	if got < max/5 {
		t.Errorf("BitrateAdvisoryFromStats() = %d, must never go below the ABR floor %d", got, max/5)
	}
}
