package pipeline

// #cgo pkg-config: gstreamer-1.0
// #include <gst/gst.h>
// #include <stdlib.h>
// #include <fcntl.h>
// #include <unistd.h>
// #include <time.h>
// #include <stdint.h>
//
// static int start_pipeline_inner(GstElement *pipeline, char **errmsg) {
//     *errmsg = NULL;
//     GstStateChangeReturn ret = gst_element_set_state(pipeline, GST_STATE_PLAYING);
//     // For live pipelines (intervideosrc, v4l2src) and hardware encoders
//     // (nvv4l2av1enc), the state change is always GST_STATE_CHANGE_ASYNC:
//     // the hardware initialises in the background after set_state returns.
//     // Waiting for ASYNC completion here holds silenceMu for up to 10 s,
//     // which serialises every concurrent Poll call (udev retries × 10 s =
//     // 30 s total startup delay). Accept ASYNC immediately: real errors are
//     // caught by watchBus.
//     if (ret == GST_STATE_CHANGE_FAILURE) {
//         // Read the first error message from the bus for a useful diagnostic.
//         GstBus *bus = gst_element_get_bus(pipeline);
//         if (bus) {
//             GstMessage *m = gst_bus_pop_filtered(bus, GST_MESSAGE_ERROR);
//             if (m) {
//                 GError *err = NULL;
//                 gst_message_parse_error(m, &err, NULL);
//                 if (err) { *errmsg = g_strdup(err->message); g_error_free(err); }
//                 gst_message_unref(m);
//             }
//             gst_object_unref(bus);
//         }
//         return 0;
//     }
//     return 1;
// }
//
// static void silence_begin(int *out, int *err) {
//     *out = dup(1);
//     *err = dup(2);
//     int nul = open("/dev/null", O_WRONLY);
//     if (*out < 0 || *err < 0 || nul < 0) {
//         if (nul >= 0) close(nul);
//         if (*out >= 0) { close(*out); *out = -1; }
//         if (*err >= 0) { close(*err); *err = -1; }
//         return;
//     }
//     dup2(nul, 1);
//     dup2(nul, 2);
//     close(nul);
// }
//
// static void silence_end(int out, int err) {
//     if (out >= 0) { dup2(out, 1); close(out); }
//     if (err >= 0) { dup2(err, 2); close(err); }
// }
//
// static GstBus* get_bus(GstElement *pipeline) {
//     return gst_element_get_bus(pipeline);
// }
//
// // Poll bus for ERROR/WARNING/EOS with 100 ms timeout.
// // Returns 1=error, 2=warning, 3=eos, 0=nothing.
// // msg and dbg must be g_free'd by caller.
// static int pop_bus_message(GstBus *bus, char **msg, char **dbg) {
//     *msg = NULL; *dbg = NULL;
//     GstMessage *m = gst_bus_timed_pop_filtered(bus, 100 * GST_MSECOND,
//         GST_MESSAGE_ERROR | GST_MESSAGE_WARNING | GST_MESSAGE_EOS);
//     if (m == NULL) return 0;
//     int ret = 0;
//     GstMessageType t = GST_MESSAGE_TYPE(m);
//     if (t == GST_MESSAGE_ERROR) {
//         ret = 1;
//         GError *err = NULL; gchar *d = NULL;
//         gst_message_parse_error(m, &err, &d);
//         if (err) { *msg = g_strdup(err->message); g_error_free(err); }
//         if (d)   { *dbg = g_strdup(d); g_free(d); }
//     } else if (t == GST_MESSAGE_WARNING) {
//         ret = 2;
//         GError *err = NULL; gchar *d = NULL;
//         gst_message_parse_warning(m, &err, &d);
//         if (err) { *msg = g_strdup(err->message); g_error_free(err); }
//         if (d)   { *dbg = g_strdup(d); g_free(d); }
//     } else if (t == GST_MESSAGE_EOS) {
//         ret = 3;
//     }
//     gst_message_unref(m);
//     return ret;
// }
//
// static void send_eos(GstElement *pipeline) {
//     gst_element_send_event(pipeline, gst_event_new_eos());
// }
//
// // set_encoder_bitrate sets the bitrate (bps) on a named encoder element.
// static void set_encoder_bitrate(GstElement *pipeline, const char *name, guint bitrate) {
//     GstElement *enc = gst_bin_get_by_name(GST_BIN(pipeline), name);
//     if (!enc) return;
//     g_object_set(enc, "bitrate", bitrate, NULL);
//     gst_object_unref(enc);
// }
//
// // set_resolution changes a named capsfilter's caps to a new width/height
// // (keeping framerate and NVMM/NV12 format) and pushes a reconfigure event
// // upstream on its sink pad. On the Jetson hardware AV1 encoder
// // (nvv4l2av1enc) placed downstream, this triggers the encoder's own DRC
// // (Dynamic Resolution Change) support: confirmed on real hardware to
// // renegotiate and keep encoding — with a new IDR/sequence header — without
// // an error, a dropped frame, or touching srtsink/the SRT connection at all.
// // See CLAUDE.md for the end-to-end measurement.
// static void set_resolution(GstElement *pipeline, const char *capsfilterName, gint width, gint height, gint framerate) {
//     GstElement *cf = gst_bin_get_by_name(GST_BIN(pipeline), capsfilterName);
//     if (!cf) return;
//     gchar *capsStr = g_strdup_printf(
//         "video/x-raw(memory:NVMM),width=%d,height=%d,framerate=%d/1,format=NV12",
//         width, height, framerate);
//     GstCaps *caps = gst_caps_from_string(capsStr);
//     g_free(capsStr);
//     g_object_set(cf, "caps", caps, NULL);
//     gst_caps_unref(caps);
//     GstPad *sinkpad = gst_element_get_static_pad(cf, "sink");
//     if (sinkpad) {
//         gst_pad_push_event(sinkpad, gst_event_new_reconfigure());
//         gst_object_unref(sinkpad);
//     }
//     gst_object_unref(cf);
// }
//
// // force_idr requests an immediate IDR frame from a named encoder element.
// // On Jetson nvv4l2* encoders, "force-IDR" is a signal, not a property:
// //   g_signal_emit_by_name (element, "force-IDR");
// // Using g_object_set for a signal name produces a GLib-CRITICAL and does
// // nothing. g_signal_lookup checks the element's type actually has a
// // "force-IDR" signal before emitting — needed since WatchLocalStats' ABR
// // loop now runs for audio (Opus) pipelines too, and calls this on every SRT
// // (re)connect regardless of encoder type; opusenc has no such signal, and
// // emitting a signal name a type doesn't declare is a GLib-CRITICAL, not a
// // silent no-op, unlike gst_bin_get_by_name returning NULL for a missing
// // element name.
// static void force_idr(GstElement *pipeline, const char *name) {
//     GstElement *enc = gst_bin_get_by_name(GST_BIN(pipeline), name);
//     if (!enc) return;
//     if (g_signal_lookup("force-IDR", G_OBJECT_TYPE(enc)) != 0) {
//         g_signal_emit_by_name(enc, "force-IDR", NULL);
//     }
//     gst_object_unref(enc);
// }
//
// // stamp_one_buffer prepends the 8-byte big-endian UTC nanosecond
// // (CLOCK_REALTIME) capture timestamp header used by both
// // prefix_timestamp_probe (single-buffer case) and stamp_buffer_list_func
// // (buffer-list case) below.
// //
// // gst_buffer_make_writable() may return a different (copied) GstBuffer than
// // the one passed in — always use its return value, never the original
// // pointer, afterward. gst_buffer_insert_memory() takes ownership of the
// // GstMemory it's given.
// static GstBuffer *stamp_one_buffer(GstBuffer *buf) {
//     struct timespec ts;
//     clock_gettime(CLOCK_REALTIME, &ts);
//     uint64_t ns = (uint64_t)ts.tv_sec * 1000000000ULL + (uint64_t)ts.tv_nsec;
//
//     GstMemory *hdr = gst_allocator_alloc(NULL, 8, NULL);
//     GstMapInfo m;
//     gst_memory_map(hdr, &m, GST_MAP_WRITE);
//     for (int i = 0; i < 8; i++) {
//         m.data[i] = (unsigned char)(ns >> ((7 - i) * 8));
//     }
//     gst_memory_unmap(hdr, &m);
//
//     buf = gst_buffer_make_writable(buf);
//     gst_buffer_insert_memory(buf, 0, hdr);
//     return buf;
// }
//
// // stamp_buffer_list_func is gst_buffer_list_foreach's per-buffer callback
// // for the GST_PAD_PROBE_TYPE_BUFFER_LIST case below: GstBaseSink's default
// // render_list behavior (which srtsink doesn't override) calls render() once
// // per buffer in the list, so each one becomes its own SRT message and needs
// // its own 8-byte prefix, not just the first.
// static gboolean stamp_buffer_list_func(GstBuffer **buffer, guint idx, gpointer user_data) {
//     *buffer = stamp_one_buffer(*buffer);
//     return TRUE;
// }
//
// // prefix_timestamp_probe prepends the capture timestamp (see
// // stamp_one_buffer) to every buffer, or every buffer within a buffer list,
// // flowing through the pad it's attached to — see attach_timestamp_probe
// // below. RaceCast-Receiver must strip these 8 bytes back off before feeding
// // the rest to its decoder; both sides have to be deployed together, since a
// // mismatched pair either corrupts every decoded frame (old receiver, new
// // emitter) or silently eats 8 bytes of real bitstream data (new receiver,
// // old emitter).
// static GstPadProbeReturn prefix_timestamp_probe(GstPad *pad, GstPadProbeInfo *info, gpointer user_data) {
//     if (info->type & GST_PAD_PROBE_TYPE_BUFFER_LIST) {
//         GstBufferList *list = GST_PAD_PROBE_INFO_BUFFER_LIST(info);
//         if (!list) return GST_PAD_PROBE_OK;
//         list = gst_buffer_list_make_writable(list);
//         gst_buffer_list_foreach(list, stamp_buffer_list_func, NULL);
//         GST_PAD_PROBE_INFO_DATA(info) = list;
//         return GST_PAD_PROBE_OK;
//     }
//
//     GstBuffer *buf = GST_PAD_PROBE_INFO_BUFFER(info);
//     if (!buf) return GST_PAD_PROBE_OK;
//     GST_PAD_PROBE_INFO_DATA(info) = stamp_one_buffer(buf);
//     return GST_PAD_PROBE_OK;
// }
//
// // attach_timestamp_probe attaches prefix_timestamp_probe to the sink pad of
// // the named element (srtsink, for stream pipelines) so every buffer is
// // stamped right before it's handed to libsrt — as close to the actual send
// // as this pipeline gets, after every encode/convert step has already run.
// // Probes both buffers and buffer lists (see prefix_timestamp_probe) since
// // an upstream element is free to push either. No-op if the named element
// // doesn't exist.
// static void attach_timestamp_probe(GstElement *pipeline, const char *name) {
//     GstElement *el = gst_bin_get_by_name(GST_BIN(pipeline), name);
//     if (!el) return;
//     GstPad *pad = gst_element_get_static_pad(el, "sink");
//     if (pad) {
//         gst_pad_add_probe(pad, GST_PAD_PROBE_TYPE_BUFFER | GST_PAD_PROBE_TYPE_BUFFER_LIST,
//             prefix_timestamp_probe, NULL, NULL);
//         gst_object_unref(pad);
//     }
//     gst_object_unref(el);
// }
//
// // try_set_intra_refresh attempts to enable intra-refresh on a named encoder.
// // Returns 1 if the encoder element was found and configured, 0 otherwise
// // (e.g. the named element does not exist in this pipeline).
// static int try_set_intra_refresh(GstElement *pipeline, const char *name, guint period) {
//     GstElement *enc = gst_bin_get_by_name(GST_BIN(pipeline), name);
//     if (!enc) return 0;
//     g_object_set(enc, "EnableIntraRefresh", (gboolean)TRUE, NULL);
//     if (period > 0)
//         g_object_set(enc, "intra-refresh-period", period, NULL);
//     gst_object_unref(enc);
//     return 1;
// }
//
// // get_srtsink_stats reads cumulative SRT statistics from a named srtsink element
// // via its "stats" GstStructure property. All counts are cumulative since the
// // connection was established; the caller computes interval deltas.
// static void get_srtsink_stats(GstElement *pipeline, const char *name,
//                                double *rtt_ms, double *bandwidth_mbps,
//                                gint64 *pkt_sent_total, gint *pkt_loss_total) {
//     *rtt_ms = 0; *bandwidth_mbps = 0; *pkt_sent_total = 0; *pkt_loss_total = 0;
//     GstElement *sink = gst_bin_get_by_name(GST_BIN(pipeline), name);
//     if (!sink) return;
//     GstStructure *stats = NULL;
//     g_object_get(sink, "stats", &stats, NULL);
//     if (stats) {
//         gst_structure_get_double(stats, "rtt-ms",         rtt_ms);
//         gst_structure_get_double(stats, "bandwidth-mbps", bandwidth_mbps);
//         gst_structure_get_int64 (stats, "packets-sent-total",      pkt_sent_total);
//         gst_structure_get_int   (stats, "packets-sent-loss-total", pkt_loss_total);
//         gst_structure_free(stats);
//     }
//     gst_object_unref(sink);
// }
//
// // set_pipeline_paused transitions the pipeline to GST_STATE_PAUSED without
// // blocking. The SRT connection is dropped; GPU encoder becomes idle.
// static void set_pipeline_paused(GstElement *pipeline) {
//     gst_element_set_state(pipeline, GST_STATE_PAUSED);
// }
//
// // set_pipeline_playing transitions the pipeline back to GST_STATE_PLAYING
// // without blocking. SRT reconnect resumes immediately.
// static void set_pipeline_playing(GstElement *pipeline) {
//     gst_element_set_state(pipeline, GST_STATE_PLAYING);
// }
import "C"

import (
	"context"
	"fmt"
	"sync"
	"time"
	"unsafe"

	"racecast-emitter/internal/logger"
)

func init() {
	C.gst_init(nil, nil)
	C.gst_debug_set_active(C.FALSE)
}

// silenceMu serialises all silence_begin/silence_end calls.
// Prevents fd-1 being permanently lost when goroutines dup(1) concurrently.
var silenceMu sync.Mutex

// GstPipeline manages a single GStreamer pipeline (source, record, or stream).
type GstPipeline struct {
	mu         sync.Mutex
	pipeline   *C.GstElement
	ctx        context.Context    // drain context — independent of the parent, cancelled only by CancelPipeline or EOS
	cancel     context.CancelFunc // cancels the drain context
	controlCtx context.Context    // parent context — used only for startup checks in activate()
	running    bool
	onError    func()
	wg         sync.WaitGroup
}

// newGstPipeline creates a GStreamer pipeline from a description string.
// parentCtx is stored as the control context for startup checks only; the
// pipeline's drain context is independent so that cancelling the parent does
// not bypass EOS-based finalisation of recording files.
func newGstPipeline(parentCtx context.Context, pipelineStr string) (*GstPipeline, error) {
	cStr := C.CString(pipelineStr)
	defer C.free(unsafe.Pointer(cStr))

	var gerr *C.GError
	gp := C.gst_parse_launch(cStr, &gerr)
	if gerr != nil {
		msg := C.GoString((*C.char)(unsafe.Pointer(gerr.message)))
		C.g_error_free(gerr)
		if gp != nil {
			// gst_parse_launch can return a non-NULL, partially-built element
			// alongside a recoverable-error GError (not just NULL+error for
			// fatal ones) — without this, that partial pipeline leaks.
			C.gst_object_unref(C.gpointer(gp))
		}
		return nil, fmt.Errorf("gst_parse_launch : %s", msg)
	}

	// Use context.Background() so that cancelling the main/parent context does
	// NOT immediately abort watchBus(). The drain context is cancelled explicitly
	// via Cancel() after EOS has propagated (or after a drain timeout).
	ctx, cancel := context.WithCancel(context.Background())
	return &GstPipeline{
		pipeline:   gp,
		ctx:        ctx,
		cancel:     cancel,
		controlCtx: parentCtx,
	}, nil
}

// SetOnError registers a callback invoked on unrecoverable pipeline errors.
func (p *GstPipeline) SetOnError(fn func()) {
	p.mu.Lock()
	p.onError = fn
	p.mu.Unlock()
}

// startInner sets the pipeline to PLAYING.
// silenceMu must be held, silence_begin called, and p.mu held by the caller.
func (p *GstPipeline) startInner() error {
	var cErr *C.char
	if C.start_pipeline_inner(p.pipeline, &cErr) == 0 {
		msg := "startup failed (unknown reason)"
		if cErr != nil {
			msg = C.GoString(cErr)
			C.g_free(C.gpointer(unsafe.Pointer(cErr)))
		}
		return fmt.Errorf("failed to start GStreamer pipeline: %s", msg)
	}
	p.running = true
	p.wg.Add(1)
	go func() { defer p.wg.Done(); p.watchBus() }()
	return nil
}

// StartEach starts all pipelines in parallel within one silence window, calling
// onReady(i, err) per pipeline as soon as it reaches GST_STATE_PLAYING (or fails).
// Faster pipelines (e.g. audio) are activated immediately while slower ones
// (e.g. nvv4l2av1enc) are still initialising.
// NOTE: onReady is called while silenceMu is held — do NOT call Free/SetNull inside
// (deadlock). Use "go gp.Free()" instead.
func StartEach(pipelines []*GstPipeline, onReady func(int, error)) {
	if len(pipelines) == 0 {
		return
	}
	silenceMu.Lock()
	defer silenceMu.Unlock()
	var out, errfd C.int
	C.silence_begin(&out, &errfd)
	defer func() {
		// Brief delay for Nvidia threads that may still write after get_state PLAYING.
		time.Sleep(100 * time.Millisecond)
		C.silence_end(out, errfd)
	}()

	var sg sync.WaitGroup
	for i, gp := range pipelines {
		i, gp := i, gp
		sg.Add(1)
		go func() {
			defer sg.Done()
			gp.mu.Lock()
			if gp.running {
				gp.mu.Unlock()
				onReady(i, nil)
				return
			}
			err := gp.startInner()
			gp.mu.Unlock()
			onReady(i, err)
		}()
	}
	sg.Wait()
}

// SendEOS injects an EOS event at the pipeline source.
func (p *GstPipeline) SendEOS() {
	p.mu.Lock()
	pip := p.pipeline
	p.mu.Unlock()
	if pip == nil {
		return
	}
	C.send_eos(pip)
}

// Pause transitions the pipeline to GST_STATE_PAUSED (non-blocking). The SRT
// connection is dropped and GPU encoder becomes idle until Play() is called.
func (p *GstPipeline) Pause() {
	p.mu.Lock()
	pip := p.pipeline
	p.mu.Unlock()
	if pip != nil {
		C.set_pipeline_paused(pip)
	}
}

// Play transitions the pipeline back to GST_STATE_PLAYING (non-blocking).
func (p *GstPipeline) Play() {
	p.mu.Lock()
	pip := p.pipeline
	p.mu.Unlock()
	if pip != nil {
		C.set_pipeline_playing(pip)
	}
}

// Cancel cancels the pipeline's internal context, causing watchBus() to exit
// immediately without waiting for an EOS bus message.
func (p *GstPipeline) Cancel() {
	p.cancel()
}

// SetNull sets the pipeline to GST_STATE_NULL (releases hardware resources).
func (p *GstPipeline) SetNull() {
	silenceMu.Lock()
	defer silenceMu.Unlock()
	var out, errfd C.int
	C.silence_begin(&out, &errfd)
	defer C.silence_end(out, errfd)
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.pipeline != nil {
		C.gst_element_set_state(p.pipeline, C.GST_STATE_NULL)
	}
}

// Free releases GStreamer resources. Call after SetNull.
func (p *GstPipeline) Free() {
	silenceMu.Lock()
	defer silenceMu.Unlock()
	var out, errfd C.int
	C.silence_begin(&out, &errfd)
	defer C.silence_end(out, errfd)
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.pipeline != nil {
		C.gst_object_unref(C.gpointer(p.pipeline))
		p.pipeline = nil
	}
}

// watchBus monitors the GStreamer bus and logs errors and warnings.
// Stops on EOS or context cancellation.
// Cancels the drain context on every exit path (EOS, error, or external
// cancellation) so that dependent watchers (e.g. WatchLocalStats' ABR loop,
// which only stops on p.ctx.Done()) don't keep running against a pipeline
// that has stopped being monitored -- e.g. after a runtime error tears the
// pipeline down without going through Slot.Stop()/Cancel().
func (p *GstPipeline) watchBus() {
	defer p.cancel()
	bus := C.get_bus(p.pipeline)
	if bus == nil {
		logger.Warn("[gst] Failed to get pipeline bus")
		return
	}
	defer C.gst_object_unref(C.gpointer(bus))

	for {
		select {
		case <-p.ctx.Done():
			return
		default:
		}
		var cMsg, cDbg *C.char
		ret := C.pop_bus_message(bus, &cMsg, &cDbg)
		msg, dbg := "", ""
		if cMsg != nil {
			msg = C.GoString(cMsg)
			C.g_free(C.gpointer(unsafe.Pointer(cMsg)))
		}
		if cDbg != nil {
			dbg = C.GoString(cDbg)
			C.g_free(C.gpointer(unsafe.Pointer(cDbg)))
		}
		switch ret {
		case 1:
			logger.Error("[gst] Pipeline error: %s -- %s", msg, dbg)
			p.mu.Lock()
			cb := p.onError
			p.mu.Unlock()
			if cb != nil {
				go cb()
			}
			return
		case 2:
			logger.Warn("[gst] Pipeline warning: %s -- %s", msg, dbg)
		case 3:
			return
		}
	}
}

// SetBitrate dynamically changes a named encoder's bitrate (bps) — the AV1
// video encoder or the Opus audio encoder, both expose a "bitrate" property.
// Safe to call while the pipeline is in PLAYING state.
func (p *GstPipeline) SetBitrate(encoderName string, bitrate int) {
	cName := C.CString(encoderName)
	defer C.free(unsafe.Pointer(cName))
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.pipeline != nil && p.running {
		C.set_encoder_bitrate(p.pipeline, cName, C.guint(bitrate))
	}
}

// SetResolution changes a named capsfilter's caps to a new resolution while
// the pipeline keeps running — see set_resolution's comment for what this
// relies on downstream. Used by bandwidth.go's resolution-tier switching
// instead of tearing down and rebuilding the pipeline: the SRT connection
// and its cumulative stats are never touched.
func (p *GstPipeline) SetResolution(capsfilterName string, width, height, framerate int) {
	cName := C.CString(capsfilterName)
	defer C.free(unsafe.Pointer(cName))
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.pipeline != nil && p.running {
		C.set_resolution(p.pipeline, cName, C.gint(width), C.gint(height), C.gint(framerate))
	}
}

// ForceIDR requests an immediate IDR frame from the named encoder. Safe to
// call on an encoder with no "force-IDR" signal (e.g. opusenc) — a no-op.
func (p *GstPipeline) ForceIDR(encoderName string) {
	cName := C.CString(encoderName)
	defer C.free(unsafe.Pointer(cName))
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.pipeline != nil && p.running {
		C.force_idr(p.pipeline, cName)
	}
}

// AttachTimestampPrefix arranges for every buffer flowing into the named
// sink element (srtsink, for stream pipelines) to be prefixed with an 8-byte
// big-endian UTC nanosecond capture timestamp — see prefix_timestamp_probe's
// comment in the cgo preamble for the wire format and the requirement that
// RaceCast-Receiver be updated to match. Must be called before Start() (i.e.
// while the pipeline is still in NULL/READY state): the intent is that every
// buffer gets stamped, not just ones lucky enough to arrive after this call
// — unlike the other named-element setters here, this has no *running guard.
func (p *GstPipeline) AttachTimestampPrefix(sinkName string) {
	cName := C.CString(sinkName)
	defer C.free(unsafe.Pointer(cName))
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.pipeline != nil {
		C.attach_timestamp_probe(p.pipeline, cName)
	}
}

// TrySetIntraRefresh attempts to enable intra-refresh on the named encoder.
// Returns true if the encoder element was found and configured. Best-effort:
// a caller that wants an accurate log should check the return value rather
// than assuming success.
func (p *GstPipeline) TrySetIntraRefresh(encoderName string, period int) bool {
	cName := C.CString(encoderName)
	defer C.free(unsafe.Pointer(cName))
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.pipeline == nil {
		return false
	}
	return C.try_set_intra_refresh(p.pipeline, cName, C.guint(period)) != 0
}

// GetSRTSinkStats reads cumulative SRT stats from the named srtsink element.
// Returns RTT (ms), bandwidth (Mbps), cumulative packets sent and lost.
func (p *GstPipeline) GetSRTSinkStats(sinkName string) (rttMS, bandwidthMbps float64, pktSentTotal int64, pktLossTotal int) {
	cName := C.CString(sinkName)
	defer C.free(unsafe.Pointer(cName))
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.pipeline == nil || !p.running {
		return
	}
	var rtt, bw C.double
	var sent C.gint64
	var lost C.gint
	C.get_srtsink_stats(p.pipeline, cName, &rtt, &bw, &sent, &lost)
	return float64(rtt), float64(bw), int64(sent), int(lost)
}
