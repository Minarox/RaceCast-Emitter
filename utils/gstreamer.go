package utils

// #cgo pkg-config: gstreamer-1.0 gstreamer-app-1.0
// #include <gst/gst.h>
// #include <gst/app/gstappsink.h>
// #include <stdlib.h>
//
// GstFlowReturn gst_app_sink_pull_sample_go(GstElement *sink, GstSample **sample) {
//     *sample = gst_app_sink_pull_sample(GST_APP_SINK(sink));
//     if (*sample == NULL) return GST_FLOW_EOS;
//     return GST_FLOW_OK;
// }
import "C"

import (
	"context"
	"fmt"
	"sync"
	"time"
	"unsafe"

	"github.com/livekit/protocol/livekit"
	lksdk "github.com/livekit/server-sdk-go/v2"
	"github.com/pion/rtp"
	"github.com/pion/webrtc/v4"
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

// NewVideoPipeline builds a GStreamer pipeline for a V4L2 video device using the
// Jetson hardware encoder (nvv4l2h264enc) and returns an RTP H264 stream via appsink.
//
// devicePath: e.g. "/dev/video0"
// width, height: capture resolution
// framerate: frames per second
func NewVideoPipeline(devicePath string, width, height, framerate int) (*GStreamerPipeline, error) {
	// v4l2src → nvv4l2camerasrc is preferred on Jetson; fall back to v4l2src.
	// The pipeline outputs RTP packetised H.264 into appsink.
	pipelineStr := fmt.Sprintf(
		"v4l2src device=%s ! "+
			"video/x-raw,width=%d,height=%d,framerate=%d/1 ! "+
			"nvvidconv ! "+
			"video/x-raw(memory:NVMM),format=NV12 ! "+
			"nvv4l2h264enc bitrate=2000000 iframeinterval=60 ! "+
			"h264parse ! "+
			"rtph264pay config-interval=-1 pt=96 ! "+
			"appsink name=sink max-buffers=2 drop=true sync=false",
		devicePath, width, height, framerate,
	)
	return newPipeline(pipelineStr)
}

// NewAudioPipeline builds a GStreamer pipeline for an ALSA audio device and returns
// an RTP Opus stream via appsink.
//
// alsaDevice: e.g. "hw:1,0"
func NewAudioPipeline(alsaDevice string) (*GStreamerPipeline, error) {
	pipelineStr := fmt.Sprintf(
		"alsasrc device=%s ! "+
			"audio/x-raw,format=S16LE,rate=48000,channels=2 ! "+
			"opusenc bitrate=96000 ! "+
			"rtpopuspay pt=111 ! "+
			"appsink name=sink max-buffers=4 drop=true sync=false",
		alsaDevice,
	)
	return newPipeline(pipelineStr)
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

// loop pulls RTP buffers from appsink and writes them to the LiveKit track.
func (p *GStreamerPipeline) loop() {
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
		if flowRet != C.GST_FLOW_OK || sample == nil {
			time.Sleep(5 * time.Millisecond)
			continue
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

		C.gst_buffer_unmap(buf, &mapInfo)
		C.gst_sample_unref(sample)

		p.mu.Lock()
		track := p.track
		p.mu.Unlock()

		if track == nil || len(data) == 0 {
			continue
		}

		// Parse as RTP packet and write to LiveKit track.
		pkt := &rtp.Packet{}
		if err := pkt.Unmarshal(data); err != nil {
			Log.Debugw("Failed to unmarshal RTP packet.", "error", err)
			continue
		}

		if err := track.WriteRTP(pkt, nil); err != nil {
			Log.Debugw("Failed to write RTP packet to track.", "error", err)
		}
	}
}

// PublishVideoTrack creates and publishes a video LocalSampleTrack to the LiveKit room.
func PublishVideoTrack(room *lksdk.Room, trackName string) (*lksdk.LocalSampleTrack, error) {
	track, err := lksdk.NewLocalSampleTrack(webrtc.RTPCodecCapability{
		MimeType:    webrtc.MimeTypeH264,
		ClockRate:   90000,
		Channels:    0,
		SDPFmtpLine: "level-asymmetry-allowed=1;packetization-mode=1;profile-level-id=42001f",
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
