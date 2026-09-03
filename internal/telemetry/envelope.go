package telemetry

// envelope.go builds the timestamped JSON envelope sent to the receiver over
// SRT and persists a per-source local copy under records/<date>/data/ — the
// same bytes for both, so the two can never drift apart.
//
// The timestamp uses UTC, matching internal/pipeline's GPS-disciplined clock
// and BWF timecode work rather than internal/logger's local-time convention:
// this ts is meant as a machine-correlatable anchor across sources (and,
// eventually, RaceCast-Receiver's planned DVR rewind, which needs a shared
// timeline across video, audio, and telemetry) rather than an
// operator-facing display value.

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// recordsDir mirrors internal/pipeline's own constant of the same name and
// value — both packages write under the same top-level directory, but
// telemetry recording has no other reason to depend on internal/pipeline.
const recordsDir = "records"

// BuildEnvelope marshals {"ts":...,"type":typ,"data":data} and returns the
// bytes alongside the UTC timestamp used, so a caller can send the exact
// same bytes over SRT (Conn.Send) and record them locally (Recorder.Write)
// without re-marshaling — and without the wire and recorded copies ever
// being able to disagree with each other.
func BuildEnvelope(typ string, data any) (payload []byte, ts time.Time, err error) {
	ts = time.Now().UTC()
	payload, err = json.Marshal(struct {
		Ts   string `json:"ts"`
		Type string `json:"type"`
		Data any    `json:"data"`
	}{
		Ts:   ts.Format(time.RFC3339Nano),
		Type: typ,
		Data: data,
	})
	return payload, ts, err
}

// Recorder appends JSON envelopes (one per line, JSONL — matching
// internal/logger's own file format so telemetry data is queryable with jq
// the same way) to records/<date>/data/<source>.jsonl, one file per
// telemetry source (e.g. "gps", "ups", and in the future the car's ECU) so
// each can be read, tailed, or imported independently. Rotates to a new
// day's file automatically, mirroring internal/logger's daily rotation.
//
// Safe for concurrent use, though in practice each source has exactly one
// goroutine calling Write (its own RunStream loop).
type Recorder struct {
	source string

	mu   sync.Mutex
	f    *os.File
	date string // "2006-01-02" of the currently open file, "" if none open
}

// NewRecorder returns a Recorder for the given source name (used verbatim as
// the file's base name, e.g. "gps" -> gps.jsonl). It opens no file until the
// first Write.
func NewRecorder(source string) *Recorder {
	return &Recorder{source: source}
}

// Write appends payload (a complete JSON object, as returned by
// BuildEnvelope) as one line, opening today's file — or rotating to it, if
// the date has changed since the last Write — as needed.
func (r *Recorder) Write(payload []byte) error {
	date := time.Now().Format("2006-01-02")

	r.mu.Lock()
	defer r.mu.Unlock()

	if r.f == nil || r.date != date {
		if r.f != nil {
			r.f.Close()
			// Nil'd immediately, not just on the success path below: if
			// MkdirAll/OpenFile then fails, r.f must not keep pointing at
			// this now-closed file — a later Close() call (at shutdown)
			// would otherwise close it a second time and surface a
			// misleading "file already closed" error for a file this
			// Recorder no longer considers open.
			r.f = nil
		}
		dir := filepath.Join(recordsDir, date, "data")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return fmt.Errorf("mkdir: %w", err)
		}
		f, err := os.OpenFile(filepath.Join(dir, r.source+".jsonl"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
		if err != nil {
			return fmt.Errorf("open: %w", err)
		}
		r.f = f
		r.date = date
	}

	if _, err := r.f.Write(payload); err != nil {
		return fmt.Errorf("write: %w", err)
	}
	if _, err := r.f.Write([]byte("\n")); err != nil {
		return fmt.Errorf("write newline: %w", err)
	}
	return nil
}

// Close closes the currently open file, if any. Safe to call even if no
// Write has happened yet.
func (r *Recorder) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.f == nil {
		return nil
	}
	err := r.f.Close()
	r.f = nil
	r.date = ""
	return err
}
