package pipeline

// frametime_export.go holds only the //export'd callback frametime.go's C
// probe calls back into — cgo requires a file using //export to have a
// preamble with no function *definitions* (gst.go's and frametime.go's both
// have plenty), so this stays split out, minimal on purpose.

// #include <stdint.h>
import "C"

import (
	"runtime/cgo"
	"time"
)

//export goFrameTimeCallback
func goFrameTimeCallback(handle C.uintptr_t) {
	v := cgo.Handle(handle).Value()
	// AttachFrameTimeProbe (frametime.go) receives its channel parameter as
	// chan<- time.Time (send-only) — that's the concrete type cgo.NewHandle
	// actually stored, even though the caller passes a bidirectional
	// chan time.Time; asserting the bidirectional type here always failed
	// silently (ok == false, nothing ever sent) until this was caught by a
	// local E2E test showing zero frametime messages ever reaching the
	// receiver despite the probe/goroutine wiring itself being confirmed
	// correct.
	ch, ok := v.(chan<- time.Time)
	if !ok {
		return
	}
	select {
	case ch <- time.Now():
	default: // forwarding goroutine can't keep up: drop, best-effort
	}
}
