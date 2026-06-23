package pipeline

import (
	"fmt"
	"net/url"
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

// srtCallerURI builds an SRT caller URI; streamid = "name:source" (e.g. "Route:camera").
func srtCallerURI(port int, name, source string) string {
	host := os.Getenv("RC_SRT_HOST")
	latency := envInt("RC_SRT_LATENCY", 800)
	streamID := name + ":" + source
	// iptos=136 = DSCP AF41: marks packets as video streaming for router QoS.
	uri := fmt.Sprintf("srt://%s:%d?streamid=%s&latency=%d&mode=caller&iptos=136", host, port, streamID, latency)
	if p := strings.TrimSpace(os.Getenv("RC_SRT_PASSPHRASE")); p != "" {
		// pbkeylen=32 → AES-256.
		uri += "&passphrase=" + url.QueryEscape(p) + "&pbkeylen=32"
	}
	return uri
}

// BuildVideoStr builds the GStreamer pipeline description for a camera.
// When srtPort == 0 (record-only): simple source → H.264 → MP4, no tee, no valves.
// When srtPort > 0: tee with two valve-gated branches:
//   - recordvalve (before record queue): closing stops recording without pipeline restart.
//   - streamvalve (after leaky stream queue): managed by WatchConnectivity at runtime.
// outputPath="/dev/null" is valid when recording is disabled (recordvalve closed).
func BuildVideoStr(cam config.Camera, dev, outputPath string, srtPort int) string {
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

	// AV1 stream encoder (reduced resolution from stream: config).
	// name=avenc: runtime bitrate/IDR control; insert-seq-hdr=true: instant reconnect.
	streamEncoder := func() string {
		if intraRefreshPeriod() > 0 {
				// Intra-refresh: periodic IDR replaced by TrySetIntraRefresh after creation.
			return fmt.Sprintf(
				"nvvidconv ! "+
					"video/x-raw(memory:NVMM),width=%d,height=%d,framerate=%d/1,format=NV12 ! "+
					"nvv4l2av1enc name=avenc control-rate=0 bitrate=%d insert-seq-hdr=true ! "+
					"av1parse ! video/x-av1,stream-format=obu-stream",
				cam.StreamWidth(), cam.StreamHeight(), cam.StreamFramerate(),
				cam.StreamBitrate(),
			)
		}
		return fmt.Sprintf(
			"nvvidconv ! "+
				"video/x-raw(memory:NVMM),width=%d,height=%d,framerate=%d/1,format=NV12 ! "+
				"nvv4l2av1enc name=avenc control-rate=0 bitrate=%d idrinterval=%d insert-seq-hdr=true ! "+
				"av1parse ! video/x-av1,stream-format=obu-stream",
			cam.StreamWidth(), cam.StreamHeight(), cam.StreamFramerate(),
			cam.StreamBitrate(), cam.StreamFramerate()/2,
		)
	}

	// SRT sink: sends the AV1 OBU stream to the server in caller mode.
	srtSink := func() string {
		return fmt.Sprintf("srtsink name=srtsink uri=%q sync=false", srtCallerURI(srtPort, cam.Name, "camera"))
	}

	// Record-only: no tee, no valves — recording is always active.
	if srtPort == 0 {
		return source + " ! " + recordBranch()
	}

	// Record + stream: tee with two valve-gated branches.
	// recordvalve sits before the record queue so that closing it (drop=true) causes
	// tee to return immediately without blocking on a full queue.
	// streamvalve sits after the leaky stream queue for symmetrical runtime control.
	return source +
		" ! tee name=t " +
		"t. ! valve name=recordvalve drop=false ! queue max-size-buffers=4 max-size-bytes=0 max-size-time=0 ! " + recordBranch() + " " +
		"t. ! queue max-size-buffers=4 max-size-bytes=0 max-size-time=0 leaky=downstream ! " +
		"valve name=streamvalve drop=false ! " + streamEncoder() + " ! " + srtSink()
}

// BuildAudioStr builds the GStreamer pipeline description for a microphone.
// When outputPath == "" (stream-only): simple source → streamvalve → Opus → SRT, no tee.
// When outputPath != "": tee with two valve-gated branches (AAC record + Opus stream).
//   - recordvalve (before record queue): always open; reserved for future button control.
//   - streamvalve (after leaky stream queue): managed by WatchConnectivity at runtime.
// When srtPort == 0 the stream branch uses fakesink (stream valve still present).
func BuildAudioStr(mic config.Microphone, alsaDev, outputPath string, srtPort int) string {
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

	// Black H.264 video track for SMPTE timecode (recording only).
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

	// Opus/SRT branch: streamvalve → Opus → SRT (or fakesink when srtPort == 0).
	opusSRTBranch := func() string {
		if srtPort == 0 {
			return "valve name=streamvalve drop=false ! fakesink"
		}
		return fmt.Sprintf(
			"valve name=streamvalve drop=false ! "+
				"opusenc bitrate=%d frame-size=20 perfect-timestamp=true ! "+
				"srtsink uri=%q sync=false",
			mic.StreamBitrate(),
			srtCallerURI(srtPort, mic.Name, "microphone"),
		)
	}

	switch {
	case outputPath != "": // record (with optional stream branch via streamvalve)
		// Always use tee so the stream valve can be enabled at runtime.
		// recordvalve sits before the record queue (fast drop when closed).
		return muxSink() + " " +
			blackTrack() + " " +
			source + " ! tee name=at " +
			"at. ! valve name=recordvalve drop=false ! queue max-size-buffers=8 max-size-bytes=0 max-size-time=0 ! " + aacBranch() + " " +
			"at. ! queue max-size-buffers=8 max-size-bytes=0 max-size-time=0 leaky=downstream ! " + opusSRTBranch()

	default: // stream-only: no MP4 mux, no black video track
		return source + " ! " + opusSRTBranch()
	}
}
