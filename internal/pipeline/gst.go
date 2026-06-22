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
//     if (ret == GST_STATE_CHANGE_ASYNC) {
//         GstState state;
//         ret = gst_element_get_state(pipeline, &state, NULL, 10 * GST_SECOND);
//     }
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

// silenceMu serialises all silence_begin/silence_end operations in this package.
// See StartAll for the detailed explanation of the race condition it prevents.
var silenceMu sync.Mutex

// GstPipeline manages a single GStreamer pipeline (capture + srtsink or filesink or both via tee).
type GstPipeline struct {
	mu          sync.Mutex
	pipeline    *C.GstElement
	pipelineStr string
	ctx         context.Context
	cancel      context.CancelFunc
	running     bool
	onError     func()
	wg          sync.WaitGroup
}

// newGstPipeline creates a GStreamer pipeline from a description string.
// The provided ctx is used as parent: cancelling it also cancels watchBus()
// even if the pipeline was just started and gp.Cancel() has not been called yet.
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

	ctx, cancel := context.WithCancel(parentCtx)
	return &GstPipeline{
		pipeline:    gp,
		pipelineStr: pipelineStr,
		ctx:         ctx,
		cancel:      cancel,
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

// StartAll starts multiple pipelines in parallel under a single global silencing window.
//
// Problem with individual concurrent starts: each goroutine would dup(1),
// redirect fd 1 to /dev/null, call get_state (~1 s), then restore fd 1.
// If G2 calls dup(1) while G1 has already redirected fd 1 to /dev/null,
// G2 saves /dev/null and restores it — stdout is permanently lost.
//
// StartAll applies a single dup2: all blocking get_state calls execute
// in parallel within this one silencing window.
// Returns errors indexed on the input slice (nil = success).
func StartAll(pipelines []*GstPipeline) []error {
	silenceMu.Lock()
	defer silenceMu.Unlock()
	var out, errfd C.int
	C.silence_begin(&out, &errfd)
	defer C.silence_end(out, errfd)

	errs := make([]error, len(pipelines))
	var sg sync.WaitGroup
	for i, gp := range pipelines {
		i, gp := i, gp
		sg.Add(1)
		go func() {
			defer sg.Done()
			gp.mu.Lock()
			defer gp.mu.Unlock()
			if gp.running {
				return
			}
			errs[i] = gp.startInner()
		}()
	}
	sg.Wait()
	// Brief delay for Nvidia threads that may still write after get_state PLAYING.
	time.Sleep(100 * time.Millisecond)
	return errs
}

// SendEOS injects an EOS event at the pipeline source.
func (p *GstPipeline) SendEOS() {
	C.send_eos(p.pipeline)
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
