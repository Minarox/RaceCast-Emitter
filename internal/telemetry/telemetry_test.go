package telemetry

import (
	"encoding/json"
	"testing"
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
