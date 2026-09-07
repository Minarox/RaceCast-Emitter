package pipeline

// frametime.go attaches a pad probe to a video stream's srtsink sink pad
// that notifies Go of each outgoing buffer's real capture time — without
// touching the buffer's bytes at all, unlike the older
// prefix_timestamp_probe (gst.go) still used for audio. Why: srtsink
// silently splits any buffer over its ~1316-byte default SRT payload size
// into multiple separate wire messages, corrupting an in-band byte-prefix
// trick for every frame split that way — virtually every encoded video
// frame (RaceCast-Receiver's CLAUDE.md, "GStreamer bridge" section,
// documents this as the actual cause of video never decoding; confirmed
// with an isolated nvv4l2av1enc → srtsink/srtsrc test with no application
// code from either repo involved). Audio frames stay well under that
// threshold and are unaffected, so they keep the older, still-correct
// mechanism unchanged. Video now delivers capture timestamps over a
// separate side-channel Conn instead — see internal/telemetry's
// SendFrameTime and this package's forwardFrameTimes (pipeline.go).

// #cgo pkg-config: gstreamer-1.0
// #include <gst/gst.h>
// #include <stdint.h>
//
// // Forward declaration of the //export'd Go callback (frametime_export.go)
// // — cgo combines every file's preamble in this package into one C
// // compilation unit, so a plain prototype here is enough for this file's
// // own C code to call it; no header include needed.
// extern void goFrameTimeCallback(uintptr_t handle);
//
// // frametime_probe calls back into Go once per buffer — or once per buffer
// // within a buffer list (GstBaseSink's default render_list calls render()
// // once per list item, same reasoning as prefix_timestamp_probe in
// // gst.go) — without inspecting or modifying the buffer at all: this probe
// // exists purely to signal "a buffer just reached this pad", nothing more,
// // so it carries zero risk of corrupting the bitstream by construction.
// static GstPadProbeReturn frametime_probe(GstPad *pad, GstPadProbeInfo *info, gpointer user_data) {
//     uintptr_t handle = (uintptr_t)user_data;
//     if (info->type & GST_PAD_PROBE_TYPE_BUFFER_LIST) {
//         GstBufferList *list = GST_PAD_PROBE_INFO_BUFFER_LIST(info);
//         if (!list) return GST_PAD_PROBE_OK;
//         guint n = gst_buffer_list_length(list);
//         for (guint i = 0; i < n; i++) {
//             goFrameTimeCallback(handle);
//         }
//         return GST_PAD_PROBE_OK;
//     }
//     if (!GST_PAD_PROBE_INFO_BUFFER(info)) return GST_PAD_PROBE_OK;
//     goFrameTimeCallback(handle);
//     return GST_PAD_PROBE_OK;
// }
//
// // attach_frametime_probe attaches frametime_probe to the sink pad of the
// // named element (srtsink, for video stream pipelines). No-op if the named
// // element doesn't exist. handle must stay valid (i.e. not Delete()d) for
// // as long as this pipeline can still push buffers through that pad.
// static void attach_frametime_probe(GstElement *pipeline, const char *name, uintptr_t handle) {
//     GstElement *el = gst_bin_get_by_name(GST_BIN(pipeline), name);
//     if (!el) return;
//     GstPad *pad = gst_element_get_static_pad(el, "sink");
//     if (pad) {
//         gst_pad_add_probe(pad, GST_PAD_PROBE_TYPE_BUFFER | GST_PAD_PROBE_TYPE_BUFFER_LIST,
//             frametime_probe, (gpointer)handle, NULL);
//         gst_object_unref(pad);
//     }
//     gst_object_unref(el);
// }
import "C"

import (
	"runtime/cgo"
	"time"
	"unsafe"
)

// AttachFrameTimeProbe attaches a probe to the named element's (srtsink)
// sink pad that sends time.Now() into ch — non-blocking, dropped if ch is
// full (e.g. pipeline.go's forwarding goroutine can't keep up) — for every
// buffer that reaches the pad. Must happen before the pipeline transitions
// to PLAYING, same requirement as AttachTimestampPrefix, and for the same
// reason (no buffer should arrive before the probe is attached). Returns a
// cgo.Handle the caller must Delete() once the pipeline is torn down (once
// nothing can call back into it any more) to avoid leaking it.
func (p *GstPipeline) AttachFrameTimeProbe(sinkName string, ch chan<- time.Time) cgo.Handle {
	handle := cgo.NewHandle(ch)
	cName := C.CString(sinkName)
	defer C.free(unsafe.Pointer(cName))
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.pipeline != nil {
		C.attach_frametime_probe(p.pipeline, cName, C.uintptr_t(handle))
	}
	return handle
}
