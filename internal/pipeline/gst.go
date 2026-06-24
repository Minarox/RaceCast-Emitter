package pipeline

// #cgo pkg-config: gstreamer-1.0
// #include <gst/gst.h>
// #include <stdlib.h>
// #include <fcntl.h>
// #include <unistd.h>
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
// // force_idr requests an immediate IDR frame from a named encoder element.
// // On Jetson nvv4l2* encoders, "force-IDR" is a signal, not a property:
// //   g_signal_emit_by_name (element, "force-IDR");
// // Using g_object_set for a signal name produces a GLib-CRITICAL and does
// // nothing. This function uses g_signal_emit_by_name unconditionally; if the
// // element has no such signal the call is silently ignored by GLib.
// static void force_idr(GstElement *pipeline, const char *name) {
//     GstElement *enc = gst_bin_get_by_name(GST_BIN(pipeline), name);
//     if (!enc) return;
//     g_signal_emit_by_name(enc, "force-IDR", NULL);
//     gst_object_unref(enc);
// }
//
// // try_set_intra_refresh attempts to enable intra-refresh on a named encoder.
// // Silently ignored if the encoder does not support the property.
// static void try_set_intra_refresh(GstElement *pipeline, const char *name, guint period) {
//     GstElement *enc = gst_bin_get_by_name(GST_BIN(pipeline), name);
//     if (!enc) return;
//     g_object_set(enc, "EnableIntraRefresh", (gboolean)TRUE, NULL);
//     if (period > 0)
//         g_object_set(enc, "intra-refresh-period", period, NULL);
//     gst_object_unref(enc);
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
// // set_valve_drop opens (drop=FALSE) or closes (drop=TRUE) a named valve element.
// static void set_valve_drop(GstElement *pipeline, const char *name, gboolean drop) {
//     GstElement *v = gst_bin_get_by_name(GST_BIN(pipeline), name);
//     if (!v) return;
//     g_object_set(v, "drop", drop, NULL);
//     gst_object_unref(v);
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

// Start sets the pipeline to PLAYING.
// For multiple concurrent pipelines without Nvidia noise, use StartAll.
func (p *GstPipeline) Start() error {
	silenceMu.Lock()
	defer silenceMu.Unlock()
	var out, errfd C.int
	C.silence_begin(&out, &errfd)
	defer C.silence_end(out, errfd)
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.running {
		return nil
	}
	return p.startInner()
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

// WaitDrain waits for internal goroutines to stop after an EOS.
// On timeout, cancels the context to force shutdown.
func (p *GstPipeline) WaitDrain(timeout time.Duration) {
	done := make(chan struct{})
	go func() { p.wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(timeout):
		p.cancel()
		p.wg.Wait()
	}
}

// SetNull sets the pipeline to GST_STATE_NULL (releases hardware resources).
func (p *GstPipeline) SetNull() {
	silenceMu.Lock()
	defer silenceMu.Unlock()
	var out, errfd C.int
	C.silence_begin(&out, &errfd)
	defer C.silence_end(out, errfd)
	C.gst_element_set_state(p.pipeline, C.GST_STATE_NULL)
}

// Stop gracefully stops the pipeline: EOS -> drain -> NULL.
func (p *GstPipeline) Stop() {
	p.mu.Lock()
	p.running = false
	p.mu.Unlock()
	p.SendEOS()
	p.WaitDrain(10 * time.Second)
	p.SetNull()
}

// Free releases GStreamer resources. Call after Stop or SetNull.
func (p *GstPipeline) Free() {
	silenceMu.Lock()
	defer silenceMu.Unlock()
	var out, errfd C.int
	C.silence_begin(&out, &errfd)
	defer C.silence_end(out, errfd)
	if p.pipeline != nil {
		C.gst_object_unref(C.gpointer(p.pipeline))
		p.pipeline = nil
	}
}

// watchBus monitors the GStreamer bus and logs errors and warnings.
// Stops on EOS or context cancellation.
func (p *GstPipeline) watchBus() {
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

// SetBitrate dynamically changes the AV1 encoder bitrate (bps).
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

// ForceIDR requests an immediate IDR frame from the named encoder.
func (p *GstPipeline) ForceIDR(encoderName string) {
	cName := C.CString(encoderName)
	defer C.free(unsafe.Pointer(cName))
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.pipeline != nil && p.running {
		C.force_idr(p.pipeline, cName)
	}
}

// TrySetIntraRefresh attempts to enable intra-refresh on the named encoder.
// Best-effort: silently ignored if the encoder does not support the properties.
func (p *GstPipeline) TrySetIntraRefresh(encoderName string, period int) {
	cName := C.CString(encoderName)
	defer C.free(unsafe.Pointer(cName))
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.pipeline != nil {
		C.try_set_intra_refresh(p.pipeline, cName, C.guint(period))
	}
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

// SetValve opens (drop=false) or closes (drop=true) a named valve element.
// No-op if the element is not found or the pipeline is not running.
func (p *GstPipeline) SetValve(name string, drop bool) {
	cName := C.CString(name)
	defer C.free(unsafe.Pointer(cName))
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.pipeline == nil || !p.running {
		return
	}
	var d C.gboolean
	if drop {
		d = C.gboolean(1)
	}
	C.set_valve_drop(p.pipeline, cName, d)
}
