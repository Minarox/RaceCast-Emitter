package logger

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestSplitPrefix(t *testing.T) {
	cases := []struct {
		in, comp, inst, rest string
	}{
		{"[abr:cam1] Bitrate 1 -> 2", "abr", "cam1", "Bitrate 1 -> 2"},
		{"[modem] state changed", "modem", "", "state changed"},
		{"[camera:front-cam:source] started", "camera", "front-cam:source", "started"},
		{"All pipelines stopped.", "", "", "All pipelines stopped."},
		{"[unterminated", "", "", "[unterminated"},
		{"[] msg", "", "", "msg"},
		{"", "", "", ""},
	}
	for _, c := range cases {
		comp, inst, rest := splitPrefix(c.in)
		if comp != c.comp || inst != c.inst || rest != c.rest {
			t.Errorf("splitPrefix(%q) = (%q,%q,%q), want (%q,%q,%q)", c.in, comp, inst, rest, c.comp, c.inst, c.rest)
		}
	}
}

func TestSplitComponent(t *testing.T) {
	cases := []struct {
		in, comp, inst string
	}{
		{"abr:cam1", "abr", "cam1"},
		{"modem", "modem", ""},
		{"camera:a:b", "camera", "a:b"},
	}
	for _, c := range cases {
		comp, inst := splitComponent(c.in)
		if comp != c.comp || inst != c.inst {
			t.Errorf("splitComponent(%q) = (%q,%q), want (%q,%q)", c.in, comp, inst, c.comp, c.inst)
		}
	}
}

// withTestFile redirects the package-level logFile/runID to a fresh temp
// file for the duration of the test, restoring the previous values after.
func withTestFile(t *testing.T) *os.File {
	t.Helper()
	dir := t.TempDir()
	f, err := os.Create(filepath.Join(dir, "test.log"))
	if err != nil {
		t.Fatalf("create temp log file: %v", err)
	}
	origFile, origRunID, origDate := logFile, runID, fileDate
	logFile, runID, fileDate = f, "testrun1", time.Now().Format("2006-01-02")
	t.Cleanup(func() {
		f.Close()
		logFile, runID, fileDate = origFile, origRunID, origDate
	})
	return f
}

func TestWriteJSONLine(t *testing.T) {
	f := withTestFile(t)

	write("INFO ", "\033[36m", "abr", "cam1", "Bitrate change", map[string]any{"from_bps": 100})

	data, err := os.ReadFile(f.Name())
	if err != nil {
		t.Fatalf("read log file: %v", err)
	}
	var e entry
	if err := json.Unmarshal(bytes.TrimSpace(data), &e); err != nil {
		t.Fatalf("unmarshal log line %q: %v", data, err)
	}
	if e.Level != "info" {
		t.Errorf("Level = %q, want %q", e.Level, "info")
	}
	if e.Component != "abr" || e.Instance != "cam1" {
		t.Errorf("Component/Instance = %q/%q, want %q/%q", e.Component, e.Instance, "abr", "cam1")
	}
	if e.Msg != "Bitrate change" {
		t.Errorf("Msg = %q, want %q", e.Msg, "Bitrate change")
	}
	if e.RunID != "testrun1" {
		t.Errorf("RunID = %q, want %q", e.RunID, "testrun1")
	}
	if got := e.Fields["from_bps"]; got != float64(100) { // JSON numbers decode as float64
		t.Errorf("Fields[\"from_bps\"] = %v, want 100", got)
	}
	if _, err := time.Parse(tsLayout, e.TS); err != nil {
		t.Errorf("TS %q does not match layout %q: %v", e.TS, tsLayout, err)
	}
}

func TestWriteOmitsEmptyComponentAndFields(t *testing.T) {
	f := withTestFile(t)

	write("WARN ", "\033[33m", "", "", "no component here", nil)

	data, err := os.ReadFile(f.Name())
	if err != nil {
		t.Fatalf("read log file: %v", err)
	}
	var raw map[string]any
	if err := json.Unmarshal(bytes.TrimSpace(data), &raw); err != nil {
		t.Fatalf("unmarshal log line %q: %v", data, err)
	}
	for _, key := range []string{"component", "instance", "fields"} {
		if _, present := raw[key]; present {
			t.Errorf("key %q present in output %s, want omitted", key, data)
		}
	}
}

func TestWriteNilLogFileDoesNotPanic(t *testing.T) {
	origFile := logFile
	logFile = nil
	defer func() { logFile = origFile }()

	write("INFO ", "\033[36m", "modem", "", "console-only", nil)
}

func TestInfoFieldsSplitsComponentShorthand(t *testing.T) {
	f := withTestFile(t)

	InfoFields("camera:front-cam", "started", map[string]any{"fps": 30})

	data, err := os.ReadFile(f.Name())
	if err != nil {
		t.Fatalf("read log file: %v", err)
	}
	var e entry
	if err := json.Unmarshal(bytes.TrimSpace(data), &e); err != nil {
		t.Fatalf("unmarshal log line %q: %v", data, err)
	}
	if e.Component != "camera" || e.Instance != "front-cam" {
		t.Errorf("Component/Instance = %q/%q, want %q/%q", e.Component, e.Instance, "camera", "front-cam")
	}
}
