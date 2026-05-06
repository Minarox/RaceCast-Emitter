package utils

// #cgo pkg-config: gstreamer-1.0 gstreamer-app-1.0
// #include <gst/gst.h>
// #include <gst/app/gstappsink.h>
// #include <stdlib.h>
//
// GstFlowReturn gst_app_sink_pull_sample_go(GstElement *sink, GstSample **sample) {
//     // Try to pull a sample with a 1-second timeout so the Go loop is never
//     // stuck indefinitely and can log warnings when no frames are arriving.
//     *sample = gst_app_sink_try_pull_sample(GST_APP_SINK(sink), GST_SECOND);
//     if (*sample == NULL) {
//         if (gst_app_sink_is_eos(GST_APP_SINK(sink))) return GST_FLOW_EOS;
//         return GST_FLOW_CUSTOM_ERROR; // timeout – no frame yet
//     }
//     return GST_FLOW_OK;
// }
//
// static GstClockTime gst_buffer_get_duration_go(GstBuffer *buf) {
//     return GST_BUFFER_DURATION_IS_VALID(buf) ? GST_BUFFER_DURATION(buf) : 0;
// }
//
// static GstBus* gst_pipeline_get_bus_go(GstElement *pipeline) {
//     return gst_element_get_bus(pipeline);
// }
//
// // Poll the bus for ERROR, WARNING or EOS with a 100 ms timeout.
// // Returns 1=error, 2=warning, 3=eos, 0=timeout/nothing.
// // msg and dbg are g_malloc'd; caller must g_free them.
// static int gst_bus_pop_message_go(GstBus *bus, char **msg, char **dbg) {
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
// // Send an upstream GstForceKeyUnit event to request an IDR frame from the encoder.
// static void gst_force_key_unit_go(GstElement *sink) {
//     GstEvent *ev = gst_event_new_custom(
//         GST_EVENT_CUSTOM_UPSTREAM,
//         gst_structure_new("GstForceKeyUnit",
//             "timestamp",    G_TYPE_UINT64,   (guint64)(GST_CLOCK_TIME_NONE),
//             "stream-time",  G_TYPE_UINT64,   (guint64)(GST_CLOCK_TIME_NONE),
//             "running-time", G_TYPE_UINT64,   (guint64)(GST_CLOCK_TIME_NONE),
//             "all-headers",  G_TYPE_BOOLEAN,  TRUE,
//             "count",        G_TYPE_UINT,     (guint)0,
//             NULL));
//     gst_element_send_event(sink, ev);
// }
import "C"

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"
	"unsafe"

	"github.com/livekit/protocol/livekit"
	lksdk "github.com/livekit/server-sdk-go/v2"
	"github.com/pion/webrtc/v4"
	"github.com/pion/webrtc/v4/pkg/media"
)

// GStreamerPipeline wraps a GStreamer pipeline and exposes RTP samples for a LiveKit track.
type GStreamerPipeline struct {
	mu          sync.Mutex
	pipeline    *C.GstElement
	appsink     *C.GstElement
	track       *lksdk.LocalSampleTrack
	pipelineStr string
	ctx         context.Context
	cancel      context.CancelFunc
	running     bool
}

// InitGStreamer must be called once before any pipeline is created.
func InitGStreamer() {
	C.gst_init(nil, nil)
}

// CameraFormat represents a V4L2 pixel format exposed by a camera.
type CameraFormat string

const (
	FormatMJPEG CameraFormat = "MJPG"
	FormatYUYV  CameraFormat = "YUYV"
)

// DetectCameraFormat queries the V4L2 device with v4l2-ctl and returns the
// best supported format. MJPEG is preferred over YUYV when both are available.
func DetectCameraFormat(device string) (CameraFormat, error) {
	out, err := exec.Command("v4l2-ctl", "--device="+device, "--list-formats").Output()
	if err != nil {
		return FormatMJPEG, fmt.Errorf("v4l2-ctl --list-formats failed for %s: %w", device, err)
	}
	s := string(out)
	if strings.Contains(s, string(FormatMJPEG)) {
		return FormatMJPEG, nil
	}
	if strings.Contains(s, string(FormatYUYV)) {
		return FormatYUYV, nil
	}
	return "", fmt.Errorf("no supported format (MJPG/YUYV) found for device %s; v4l2-ctl output: %s", device, s)
}

// PipelineConfig holds the configuration for a video capture pipeline.
type PipelineConfig struct {
	Name      string
	Device    string
	Width     int
	Height    int
	Framerate int
	Bitrate   int
}

// NewVideoPipeline builds a GStreamer VP9 pipeline from a V4L2 MJPEG camera or
// a SMPTE test pattern, depending on fakeStream.
// LiveKit/pion handles RTP packetisation via WriteSample.
func NewVideoPipeline(cfg PipelineConfig, fakeStream bool) (*GStreamerPipeline, error) {
	if cfg.Bitrate <= 0 {
		cfg.Bitrate = 2_000_000
	}

	var pipelineStr string
	if fakeStream {
		pipelineStr = fmt.Sprintf(
			"videotestsrc pattern=smpte is-live=true ! "+
				"video/x-raw,format=I420,width=%d,height=%d,framerate=%d/1 ! "+
				"nvvidconv ! "+
				"video/x-raw(memory:NVMM),format=NV12 ! "+
			"nvv4l2vp9enc bitrate=%d iframeinterval=60 ! "+
			"appsink name=sink max-buffers=2 drop=true sync=false",
			cfg.Width, cfg.Height, cfg.Framerate, cfg.Bitrate,
		)
	} else {
		if err := checkVideoDevice(cfg.Device); err != nil {
			return nil, err
		}
		camFmt, err := DetectCameraFormat(cfg.Device)
		if err != nil {
			return nil, err
		}
		Log.Infow("Camera format detected.", "device", cfg.Device, "format", camFmt)
		switch camFmt {
		case FormatMJPEG:
			pipelineStr = buildMJPEGPipeline(cfg)
		case FormatYUYV:
			pipelineStr = buildYUYVPipeline(cfg)
		default:
			return nil, fmt.Errorf("unsupported camera format: %s", camFmt)
		}
	}

	Log.Debugw("GStreamer video pipeline created.", "pipeline", pipelineStr)
	return newPipeline(pipelineStr)
}

// NewAudioPipeline builds a GStreamer pipeline for an ALSA audio device and returns
// an RTP Opus stream via appsink.
//
// alsaDevice: e.g. "hw:1,0"
func NewAudioPipeline(alsaDevice string, fakeStream bool) (*GStreamerPipeline, error) {
	var pipelineStr string
	if fakeStream {
		pipelineStr = fmt.Sprintf(
			"audiotestsrc wave=sine freq=440 is-live=true ! "+
				"audio/x-raw,format=S16LE,rate=48000,channels=2 ! "+
				"opusenc bitrate=96000 ! "+
				"rtpopuspay pt=111 ! "+
				"appsink name=sink max-buffers=4 drop=true sync=false",
		)
	} else {
		if err := checkAudioDevice(alsaDevice); err != nil {
			return nil, err
		}
		pipelineStr = fmt.Sprintf(
			"alsasrc device=%s ! "+
				"audio/x-raw,format=S16LE,rate=48000,channels=2 ! "+
				"opusenc bitrate=96000 ! "+
				"rtpopuspay pt=111 ! "+
				"appsink name=sink max-buffers=4 drop=true sync=false",
			alsaDevice,
		)
	}

	Log.Debugw("GStreamer audio pipeline created.", "pipeline", pipelineStr)
	return newPipeline(pipelineStr)
}

// buildMJPEGPipeline returns a GStreamer pipeline string for MJPEG cameras.
func buildMJPEGPipeline(cfg PipelineConfig) string {
	return fmt.Sprintf(
		"v4l2src device=%s ! "+
			"image/jpeg,width=%d,height=%d,framerate=%d/1 ! "+
			"nvv4l2decoder mjpeg=1 ! "+
			"nvvidconv ! "+
			"video/x-raw(memory:NVMM),format=NV12 ! "+
			"nvv4l2vp9enc bitrate=%d iframeinterval=60 ! "+
			"appsink name=sink max-buffers=2 drop=true sync=false",
		cfg.Device, cfg.Width, cfg.Height, cfg.Framerate, cfg.Bitrate,
	)
}

// buildYUYVPipeline returns a GStreamer pipeline string for YUYV cameras.
// YUYV (YUY2) is converted to I420 via videoconvert before the NVMM encoder.
func buildYUYVPipeline(cfg PipelineConfig) string {
	return fmt.Sprintf(
		"v4l2src device=%s ! "+
			"video/x-raw,format=YUY2,width=%d,height=%d,framerate=%d/1 ! "+
			"videoconvert ! "+
			"video/x-raw,format=I420 ! "+
			"nvvidconv ! "+
			"video/x-raw(memory:NVMM),format=NV12 ! "+
			"nvv4l2vp9enc bitrate=%d iframeinterval=60 ! "+
			"appsink name=sink max-buffers=2 drop=true sync=false",
		cfg.Device, cfg.Width, cfg.Height, cfg.Framerate, cfg.Bitrate,
	)
}

// checkVideoDevice verifies that the V4L2 device node exists and is readable.
func checkVideoDevice(devicePath string) error {
	info, err := os.Stat(devicePath)
	if err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("video device not found: %s", devicePath)
		}
		return fmt.Errorf("cannot stat video device %s: %w", devicePath, err)
	}
	Log.Infow("Video device found.", "device", devicePath, "mode", info.Mode())

	f, err := os.OpenFile(devicePath, os.O_RDWR, 0)
	if err != nil {
		return fmt.Errorf("cannot open video device %s (check permissions / video group membership): %w", devicePath, err)
	}
	f.Close()
	return nil
}

// checkAudioDevice verifies that the ALSA device node exists and is readable.
func checkAudioDevice(device string) error {
	if _, err := os.Stat("/dev/snd/" + device); err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("audio device not found: %s", device)
		}
		return fmt.Errorf("cannot stat audio device %s: %w", device, err)
	}
	return nil
}

func newPipeline(pipelineStr string) (*GStreamerPipeline, error) {
	cStr := C.CString(pipelineStr)
	defer C.free(unsafe.Pointer(cStr))

	var gerr *C.GError
	pipeline := C.gst_parse_launch(cStr, &gerr)
	if gerr != nil {
		errMsg := C.GoString((*C.char)(unsafe.Pointer(gerr.message)))
		C.g_error_free(gerr)
		return nil, fmt.Errorf("gst_parse_launch: %s", errMsg)
	}

	sinkName := C.CString("sink")
	defer C.free(unsafe.Pointer(sinkName))
	appsink := C.gst_bin_get_by_name((*C.GstBin)(unsafe.Pointer(pipeline)), sinkName)
	if appsink == nil {
		C.gst_object_unref(C.gpointer(pipeline))
		return nil, fmt.Errorf("appsink element not found in pipeline")
	}

	ctx, cancel := context.WithCancel(context.Background())
	return &GStreamerPipeline{
		pipeline:    pipeline,
		appsink:     appsink,
		pipelineStr: pipelineStr,
		ctx:         ctx,
		cancel:      cancel,
	}, nil
}

// ForceKeyframe sends an upstream GstForceKeyUnit event to the encoder,
// requesting an IDR frame on the next encoded buffer.
// Call this whenever a new subscriber connects to minimise time-to-first-frame.
func (p *GStreamerPipeline) ForceKeyframe() {
	p.mu.Lock()
	appsink := p.appsink
	running := p.running
	p.mu.Unlock()

	if !running || appsink == nil {
		return
	}
	C.gst_force_key_unit_go(appsink)
}

// AttachTrack associates a LiveKit LocalSampleTrack with the pipeline.
func (p *GStreamerPipeline) AttachTrack(track *lksdk.LocalSampleTrack) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.track = track
}

// Start sets the pipeline to PLAYING and begins pumping RTP buffers to the track.
func (p *GStreamerPipeline) Start() error {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.running {
		return nil
	}

	ret := C.gst_element_set_state(p.pipeline, C.GST_STATE_PLAYING)
	if ret == C.GST_STATE_CHANGE_FAILURE {
		return fmt.Errorf("failed to set pipeline to PLAYING")
	}

	p.running = true
	go p.loop()
	go p.watchBus()
	return nil
}

// Stop tears down the pipeline gracefully.
func (p *GStreamerPipeline) Stop() {
	p.mu.Lock()
	cancel := p.cancel
	p.running = false
	p.mu.Unlock()

	cancel()
	C.gst_element_set_state(p.pipeline, C.GST_STATE_NULL)
}

// Free releases GStreamer resources. Must be called after Stop.
func (p *GStreamerPipeline) Free() {
	if p.appsink != nil {
		C.gst_object_unref(C.gpointer(p.appsink))
		p.appsink = nil
	}
	if p.pipeline != nil {
		C.gst_object_unref(C.gpointer(p.pipeline))
		p.pipeline = nil
	}
}

// watchBus reads ERROR, WARNING and EOS messages from the GStreamer pipeline bus
// and logs them so that codec / device / caps negotiation failures are visible.
func (p *GStreamerPipeline) watchBus() {
	bus := C.gst_pipeline_get_bus_go(p.pipeline)
	if bus == nil {
		Log.Warnw("GStreamer: could not get pipeline bus.")
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
		ret := C.gst_bus_pop_message_go(bus, &cMsg, &cDbg)

		msg, dbg := "", ""
		if cMsg != nil {
			msg = C.GoString(cMsg)
			C.g_free(C.gpointer(cMsg))
		}
		if cDbg != nil {
			dbg = C.GoString(cDbg)
			C.g_free(C.gpointer(cDbg))
		}

		switch ret {
		case 1:
			Log.Errorw("GStreamer pipeline error.", "message", msg, "debug", dbg)
		case 2:
			Log.Warnw("GStreamer pipeline warning.", "message", msg, "debug", dbg)
		case 3:
			Log.Infow("GStreamer bus received EOS.")
			return
		}
	}
}

// loop pulls encoded frames from appsink and writes them to the LiveKit track.
// Uses a 1-second timeout on each pull to detect and log stalls.
func (p *GStreamerPipeline) loop() {
	var writeWarned  bool

	for {
		select {
		case <-p.ctx.Done():
			return
		default:
		}

		var sample *C.GstSample
		flowRet := C.gst_app_sink_pull_sample_go(p.appsink, &sample)

		if flowRet == C.GST_FLOW_EOS {
			Log.Infow("GStreamer pipeline reached EOS.")
			return
		}

		buf := C.gst_sample_get_buffer(sample)
		if buf == nil {
			C.gst_sample_unref(sample)
			continue
		}

		var mapInfo C.GstMapInfo
		if C.gst_buffer_map(buf, &mapInfo, C.GST_MAP_READ) == C.gboolean(0) {
			C.gst_sample_unref(sample)
			continue
		}

		data := C.GoBytes(unsafe.Pointer(mapInfo.data), C.int(mapInfo.size))

		// Read duration before unmapping/unreffing.
		dur := time.Duration(C.gst_buffer_get_duration_go(buf))
		if dur <= 0 {
			dur = time.Second / 30
		}

		C.gst_buffer_unmap(buf, &mapInfo)
		C.gst_sample_unref(sample)

		p.mu.Lock()
		track := p.track
		p.mu.Unlock()

		if track == nil || len(data) == 0 {
			continue
		}

		// Write the raw VP9 bitstream as a media sample; LiveKit/pion handles
		// RTP packetisation (SSRC, PT, sequence numbers) internally.
		if err := track.WriteSample(media.Sample{Data: data, Duration: dur}, nil); err != nil {
			if !writeWarned {
				Log.Warnw("GStreamer: failed to write sample to LiveKit track (further errors suppressed).", "error", err)
				writeWarned = true
			}
		}
	}
}

// PublishVideoTrack creates and publishes a video LocalSampleTrack to the LiveKit room.
func PublishVideoTrack(room *lksdk.Room, trackName string) (*lksdk.LocalSampleTrack, error) {
	track, err := lksdk.NewLocalSampleTrack(webrtc.RTPCodecCapability{
		MimeType:  webrtc.MimeTypeVP9,
		ClockRate: 90000,
		Channels:  0,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to create video sample track: %w", err)
	}

	_, err = room.LocalParticipant.PublishTrack(track, &lksdk.TrackPublicationOptions{
		Name:   trackName,
		Source: livekit.TrackSource_CAMERA,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to publish video track: %w", err)
	}

	return track, nil
}

// PublishAudioTrack creates and publishes an audio LocalSampleTrack to the LiveKit room.
func PublishAudioTrack(room *lksdk.Room, trackName string) (*lksdk.LocalSampleTrack, error) {
	track, err := lksdk.NewLocalSampleTrack(webrtc.RTPCodecCapability{
		MimeType:  webrtc.MimeTypeOpus,
		ClockRate: 48000,
		Channels:  2,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to create audio sample track: %w", err)
	}

	_, err = room.LocalParticipant.PublishTrack(track, &lksdk.TrackPublicationOptions{
		Name:   trackName,
		Source: livekit.TrackSource_MICROPHONE,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to publish audio track: %w", err)
	}

	return track, nil
}
