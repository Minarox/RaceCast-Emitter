package telemetry

import (
	"encoding/json"
	"testing"
	"time"
)

func TestStreamCloseMessage(t *testing.T) {
	got := streamCloseMessage("Habitacle:camera")

	var decoded struct {
		Type   string `json:"type"`
		Stream string `json:"stream"`
	}
	if err := json.Unmarshal(got, &decoded); err != nil {
		t.Fatalf("streamCloseMessage produced invalid JSON: %v\n%s", err, got)
	}
	if decoded.Type != "stream_close" {
		t.Errorf("type = %q, want %q", decoded.Type, "stream_close")
	}
	if decoded.Stream != "Habitacle:camera" {
		t.Errorf("stream = %q, want %q (full name:source key, not just the bare name)", decoded.Stream, "Habitacle:camera")
	}
}

func TestStreamCloseMessageEscapesStreamKey(t *testing.T) {
	// The receiver's own Name validation already rejects quote/control
	// characters, but streamCloseMessage should still produce valid JSON for
	// any input rather than relying solely on that upstream guarantee.
	got := streamCloseMessage(`weird"name`)
	var decoded struct {
		Stream string `json:"stream"`
	}
	if err := json.Unmarshal(got, &decoded); err != nil {
		t.Fatalf("streamCloseMessage produced invalid JSON for a quote-containing key: %v\n%s", err, got)
	}
	if decoded.Stream != `weird"name` {
		t.Errorf("stream = %q, want %q", decoded.Stream, `weird"name`)
	}
}

func TestFrameTimeMessage(t *testing.T) {
	ts := time.Date(2026, 9, 4, 15, 30, 0, 123456789, time.UTC)
	got := frameTimeMessage("Route:camera", 42, ts)

	var decoded struct {
		Type   string `json:"type"`
		Stream string `json:"stream"`
		Seq    uint64 `json:"seq"`
		TS     string `json:"ts"`
	}
	if err := json.Unmarshal(got, &decoded); err != nil {
		t.Fatalf("frameTimeMessage produced invalid JSON: %v\n%s", err, got)
	}
	if decoded.Type != "frametime" {
		t.Errorf("type = %q, want %q", decoded.Type, "frametime")
	}
	if decoded.Stream != "Route:camera" {
		t.Errorf("stream = %q, want %q", decoded.Stream, "Route:camera")
	}
	if decoded.Seq != 42 {
		t.Errorf("seq = %d, want 42", decoded.Seq)
	}
	gotTS, err := time.Parse(time.RFC3339Nano, decoded.TS)
	if err != nil {
		t.Fatalf("ts %q did not parse as RFC3339Nano: %v", decoded.TS, err)
	}
	if !gotTS.Equal(ts) {
		t.Errorf("ts = %v, want %v", gotTS, ts)
	}
}

func TestFrameTimeMessageEscapesStreamKey(t *testing.T) {
	got := frameTimeMessage(`weird"name`, 1, time.Now())
	var decoded struct {
		Stream string `json:"stream"`
	}
	if err := json.Unmarshal(got, &decoded); err != nil {
		t.Fatalf("frameTimeMessage produced invalid JSON for a quote-containing key: %v\n%s", err, got)
	}
	if decoded.Stream != `weird"name` {
		t.Errorf("stream = %q, want %q", decoded.Stream, `weird"name`)
	}
}
