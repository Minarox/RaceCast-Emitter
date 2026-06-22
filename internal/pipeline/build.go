package pipeline

import (
	"fmt"
	"os"
	"strings"

	"racecast-emitter/internal/config"
)

// flipMethod returns the nvvidconv flip-method index.
// 0=none, 2=rotate-180, 4=horizontal-flip, 6=vertical-flip
func flipMethod(vertical, horizontal bool) int {
	switch {
	case vertical && horizontal:
		return 2
	case horizontal:
		return 4
	case vertical:
		return 6
	default:
		return 0
	}
}

// srtCallerURI builds an SRT URI in caller mode (Jetson → server).
// streamid encodes device metadata as "name:source" (e.g. "Route:camera");
// the receiver reads it from SRTO_STREAMID to create LiveKit tracks automatically.
func srtCallerURI(port int, name, source string) string {
	host := os.Getenv("RC_SRT_HOST")
	latency := envInt("RC_SRT_LATENCY", 2000)
	streamID := name + ":" + source
	return fmt.Sprintf("srt://%s:%d?streamid=%s&latency=%d&mode=caller", host, port, streamID, latency)
}

// BuildVideoStr builds the GStreamer pipeline description for a camera.
//
//   - outputPath=="" → SRT AV1 stream only (srtsink)
//   - doStream==false → H.264 MP4 recording only (filesink)
//   - both            → shared encoder via tee:
//     record branch → mp4mux → filesink
//     stream branch → srtsink (raw AV1 OBU stream)
func BuildVideoStr(cam config.Camera, dev, outputPath string, doStream bool, srtPort int) string {
	flip := flipMethod(cam.VerticalFlip, cam.HorizontalFlip)

	// Common source chain: v4l2 → decode → flip → NVMM NV12
	var source string
	switch strings.ToUpper(cam.Format) {
	case "YUY2", "YUYV":
		source = fmt.Sprintf(
			"v4l2src device=%s do-timestamp=true ! "+
				"video/x-raw,width=%d,height=%d,framerate=%d/1 ! "+
				"timecodestamper source=rtc ! "+
				"nvvidconv flip-method=%d ! "+
				"video/x-raw(memory:NVMM),format=NV12",
			dev, cam.Width, cam.Height, cam.Framerate, flip,
		)
	default: // MJPEG
		source = fmt.Sprintf(
			"v4l2src device=%s do-timestamp=true ! "+
				"image/jpeg,width=%d,height=%d,framerate=%d/1 ! "+
				"nvv4l2decoder mjpeg=true enable-max-performance=true ! "+
				"nvvidconv flip-method=%d ! "+
				"video/x-raw,format=NV12 ! "+
				"timecodestamper source=rtc ! "+
				"nvvidconv ! "+
				"video/x-raw(memory:NVMM),format=NV12",
			dev, cam.Width, cam.Height, cam.Framerate, flip,
		)
	}

	// Recording branch: H.264 → fragmented MP4 (high quality).
	recordBranch := func() string {
		return fmt.Sprintf(
			"nvv4l2h264enc bitrate=%d idrinterval=%d insert-sps-pps=true profile=4 ! "+
				"h264parse ! mp4mux fragment-duration=500 ! "+
				"filesink location=%s sync=false",
			videoBitrate(), cam.Framerate/2, outputPath,
		)
	}

	// Stream AV1 encoder (reduced resolution/bitrate from stream: config).
	// insert-seq-hdr=true: each IDR embeds the AV1 sequence header → instant reconnect.
	// av1parse align=tu: each buffer = one temporal unit = one complete frame.
	streamEncoder := func() string {
		return fmt.Sprintf(
			"nvvidconv ! "+
				"video/x-raw(memory:NVMM),width=%d,height=%d,framerate=%d/1,format=NV12 ! "+
				"nvv4l2av1enc bitrate=%d idrinterval=%d insert-seq-hdr=true ! "+
				"av1parse ! video/x-av1,stream-format=obu-stream",
			cam.StreamWidth(), cam.StreamHeight(), cam.StreamFramerate(),
			cam.StreamBitrate(), cam.StreamFramerate()/2,
		)
	}

	// SRT sink: sends the AV1 OBU stream to the server in caller mode.
	srtSink := func() string {
		return fmt.Sprintf("srtsink uri=%q sync=false", srtCallerURI(srtPort, cam.Name, "camera"))
	}

	switch {
	case outputPath != "" && !doStream:
		// Record only: source → H.264 encoder → MP4
		return source + " ! " + recordBranch()

	case outputPath == "" && doStream:
		// Stream only: source → AV1 encoder → SRT
		return source + " ! " + streamEncoder() + " ! " + srtSink()

	default: // record + stream simultaneously
		// Two independent HW encoders: separate quality/resolution.
		return source +
			" ! tee name=t " +
			"t. ! queue max-size-buffers=4 max-size-bytes=0 max-size-time=0 ! " + recordBranch() + " " +
			"t. ! queue max-size-buffers=4 max-size-bytes=0 max-size-time=0 leaky=downstream ! " +
			streamEncoder() + " ! " + srtSink()
	}
}

// BuildAudioStr builds the GStreamer pipeline description for a microphone.
//
//   - outputPath=="" → raw Opus over SRT only
//   - doStream==false → AAC MP4 recording only (with black SMPTE video track)
//   - both            → tee on the audio source:
//     record branch → avenc_aac → mp4mux → filesink
//     stream branch → opusenc → srtsink (raw Opus, no container)
func BuildAudioStr(mic config.Microphone, alsaDev, outputPath string, doStream bool, srtPort int) string {
	const (
		blackFramerate = 25
		blackBitrate   = 100_000
	)

	// Common audio source
	source := fmt.Sprintf(
		"alsasrc device=%s do-timestamp=true ! "+
			"audio/x-raw,rate=%d,channels=%d ! "+
			"audioconvert",
		alsaDev, mic.SampleRate, mic.Channels,
	)

	// Black video track for SMPTE timecode (recording only).
	blackTrack := func() string {
		return fmt.Sprintf(
			"videotestsrc pattern=black is-live=true ! "+
				"video/x-raw,width=320,height=240,framerate=%d/1 ! "+
				"timecodestamper source=rtc ! "+
				"nvvidconv ! video/x-raw(memory:NVMM),format=NV12 ! "+
				"nvv4l2h264enc bitrate=%d idrinterval=%d insert-sps-pps=true profile=4 ! "+
				"h264parse ! mux.",
			blackFramerate, blackBitrate, blackFramerate/2,
		)
	}

	// MP4 muxer + filesink
	muxSink := func() string {
		return fmt.Sprintf(
			"mp4mux name=mux fragment-duration=500 ! filesink location=%s sync=false",
			outputPath,
		)
	}

	// AAC branch → mux (recording)
	aacBranch := func() string {
		return fmt.Sprintf(
			"audioconvert ! avenc_aac bitrate=%d ! aacparse ! mux.",
			audioBitrate(),
		)
	}

	// Opus → SRT branch (streaming): raw Opus, each SRT message = one Opus frame.
	opusSRTBranch := func() string {
		return fmt.Sprintf(
			"opusenc bitrate=%d frame-size=20 perfect-timestamp=true ! "+
				"srtsink uri=%q sync=false",
			mic.StreamBitrate(),
			srtCallerURI(srtPort, mic.Name, "microphone"),
		)
	}

	switch {
	case outputPath != "" && !doStream:
		return muxSink() + " " +
			blackTrack() + " " +
			source + " ! " + aacBranch()

	case outputPath == "" && doStream:
		return source + " ! " + opusSRTBranch()

	default: // record + stream via tee
		return muxSink() + " " +
			blackTrack() + " " +
			source + " ! tee name=at " +
			"at. ! queue max-size-buffers=8 max-size-bytes=0 max-size-time=0 ! " + aacBranch() + " " +
			"at. ! queue max-size-buffers=8 max-size-bytes=0 max-size-time=0 leaky=downstream ! " + opusSRTBranch()
	}
}
