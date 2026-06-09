package scripts

import (
	"fmt"
	"os"
	"time"

	"racecast-emitter/utils"
)

// RecordManager controls whether recording is active and provides file paths
// for each device.
//
// Recording is activated by setting the RECORD_ENABLED environment variable to
// "1" or "true" before launch. A future GPIO-button integration can replace or
// extend this behaviour at runtime.
type RecordManager struct {
	enabled bool
	dir     string // e.g. "records/2024-01-15"
}

// NewRecordManager reads RECORD_ENABLED and, when set, creates the
// records/YYYY-MM-DD directory for today's date.
func NewRecordManager() *RecordManager {
	rm := &RecordManager{}
	v := os.Getenv("RECORD_ENABLED")
	if v != "1" && v != "true" {
		utils.Log.Infow("Recording disabled (set RECORD_ENABLED=1 to enable).")
		return rm
	}

	today := time.Now().Format("2006-01-02")
	rm.dir = fmt.Sprintf("records/%s", today)
	if err := os.MkdirAll(rm.dir, 0o755); err != nil {
		utils.Log.Errorw("Failed to create records directory; recording disabled.",
			"dir", rm.dir, "error", err)
		rm.dir = ""
		return rm
	}

	rm.enabled = true
	utils.Log.Infow("Recording enabled.", "dir", rm.dir)
	return rm
}

// Enabled reports whether recording is active.
func (rm *RecordManager) Enabled() bool { return rm.enabled }

// Dir returns the records directory for today (e.g. "records/2024-01-15").
func (rm *RecordManager) Dir() string { return rm.dir }

// PathFor builds the output file path for a device with the given name and kind.
// The filename format is "{name}_{video|audio}_{HH-MM-SS}.mkv".
func (rm *RecordManager) PathFor(name, kind string) string {
	if !rm.enabled {
		return ""
	}
	ts := time.Now().Format("15-04-05")
	return fmt.Sprintf("%s/%s_%s_%s.mkv", rm.dir, name, kind, ts)
}
