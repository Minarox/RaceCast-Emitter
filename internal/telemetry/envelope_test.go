package telemetry

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestBuildEnvelope(t *testing.T) {
	before := time.Now().UTC()
	payload, ts, err := BuildEnvelope("ups", struct {
		V float64 `json:"v"`
	}{V: 12.6})
	after := time.Now().UTC()
	if err != nil {
		t.Fatalf("BuildEnvelope: %v", err)
	}

	if ts.Before(before) || ts.After(after) {
		t.Errorf("ts %v not within [%v, %v]", ts, before, after)
	}
	if ts.Location() != time.UTC {
		t.Errorf("ts location = %v, want UTC", ts.Location())
	}

	var decoded struct {
		Ts   string          `json:"ts"`
		Type string          `json:"type"`
		Data json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(payload, &decoded); err != nil {
		t.Fatalf("unmarshal payload: %v", err)
	}
	if decoded.Type != "ups" {
		t.Errorf("type = %q, want %q", decoded.Type, "ups")
	}
	if !strings.Contains(decoded.Ts, "Z") {
		t.Errorf("ts %q does not look like UTC RFC3339 (no Z suffix)", decoded.Ts)
	}
	parsedTs, err := time.Parse(time.RFC3339Nano, decoded.Ts)
	if err != nil {
		t.Fatalf("ts %q not valid RFC3339Nano: %v", decoded.Ts, err)
	}
	if !parsedTs.Equal(ts) {
		t.Errorf("parsed ts %v != returned ts %v", parsedTs, ts)
	}
	if !strings.Contains(string(decoded.Data), `"v":12.6`) {
		t.Errorf("data field missing expected content: %s", decoded.Data)
	}
}

// readJSONL reads every line of a JSONL file as raw JSON messages, in order.
func readJSONL(t *testing.T, path string) []map[string]any {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	defer f.Close()

	var out []map[string]any
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := sc.Text()
		if line == "" {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			t.Fatalf("line %q is not valid JSON: %v", line, err)
		}
		out = append(out, m)
	}
	if err := sc.Err(); err != nil {
		t.Fatalf("scan %s: %v", path, err)
	}
	return out
}

func TestRecorder_WritesJSONLUnderTodaysDataDir(t *testing.T) {
	t.Chdir(t.TempDir())

	rec := NewRecorder("gps")
	defer rec.Close()

	for i := 0; i < 3; i++ {
		payload, _, err := BuildEnvelope("modem", struct {
			N int `json:"n"`
		}{N: i})
		if err != nil {
			t.Fatal(err)
		}
		if err := rec.Write(payload); err != nil {
			t.Fatalf("Write: %v", err)
		}
	}

	wantDir := filepath.Join(recordsDir, time.Now().Format("2006-01-02"), "data")
	wantPath := filepath.Join(wantDir, "gps.jsonl")
	if _, err := os.Stat(wantPath); err != nil {
		t.Fatalf("expected file %s to exist: %v", wantPath, err)
	}

	lines := readJSONL(t, wantPath)
	if len(lines) != 3 {
		t.Fatalf("got %d lines, want 3", len(lines))
	}
	for i, line := range lines {
		data, ok := line["data"].(map[string]any)
		if !ok {
			t.Fatalf("line %d: data field missing or wrong type: %v", i, line)
		}
		if int(data["n"].(float64)) != i {
			t.Errorf("line %d: n = %v, want %d", i, data["n"], i)
		}
		if line["type"] != "modem" {
			t.Errorf("line %d: type = %v, want modem", i, line["type"])
		}
		if _, ok := line["ts"].(string); !ok {
			t.Errorf("line %d: ts field missing", i)
		}
	}
}

func TestRecorder_SeparatesSourcesIntoDistinctFiles(t *testing.T) {
	t.Chdir(t.TempDir())

	gpsRec := NewRecorder("gps")
	defer gpsRec.Close()
	upsRec := NewRecorder("ups")
	defer upsRec.Close()

	gpsPayload, _, _ := BuildEnvelope("modem", struct{}{})
	upsPayload, _, _ := BuildEnvelope("ups", struct{}{})

	if err := gpsRec.Write(gpsPayload); err != nil {
		t.Fatal(err)
	}
	if err := upsRec.Write(upsPayload); err != nil {
		t.Fatal(err)
	}

	dataDir := filepath.Join(recordsDir, time.Now().Format("2006-01-02"), "data")
	gpsLines := readJSONL(t, filepath.Join(dataDir, "gps.jsonl"))
	upsLines := readJSONL(t, filepath.Join(dataDir, "ups.jsonl"))

	if len(gpsLines) != 1 || gpsLines[0]["type"] != "modem" {
		t.Errorf("gps.jsonl = %v, want exactly one modem-type line", gpsLines)
	}
	if len(upsLines) != 1 || upsLines[0]["type"] != "ups" {
		t.Errorf("ups.jsonl = %v, want exactly one ups-type line", upsLines)
	}
}

func TestRecorder_CloseWithoutWriteIsSafe(t *testing.T) {
	rec := NewRecorder("gps")
	if err := rec.Close(); err != nil {
		t.Errorf("Close before any Write: %v", err)
	}
	if err := rec.Close(); err != nil {
		t.Errorf("second Close: %v", err)
	}
}
