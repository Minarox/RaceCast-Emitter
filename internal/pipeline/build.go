package pipeline

import (
	"fmt"
	"net/url"
	"os"
	"strings"

	"racecast-emitter/internal/config"
)

// ── Helpers ──────────────────────────────────────────────────────────────────

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

// interChannel returns the inter-element channel names for a source.
// prefix: "v" for video, "a" for audio.
// The "rc:" namespace prevents collisions with other inter-element users on the system.
func interChannel(prefix, name string) (rec, str string) {
	s := sanitize(name)
	return "rc:" + prefix + "rec:" + s, "rc:" + prefix + "str:" + s
}

// ── Video pipelines ──────────────────────────────────────────────────────────

// BuildVideoSourceStr builds the always-running capture pipeline for a camera.
// It decodes the device stream, applies flip, then feeds two inter-element channels:
// one for the recording consumer and one for the streaming consumer.
// Inter-element sinks drop frames silently when no consumer pipeline is running.
func BuildVideoSourceStr(cam config.Camera, dev string) string {
	recCh, strCh := interChannel("v", cam.Name)
	flip := flipMethod(cam.VerticalFlip, cam.HorizontalFlip)
	// I420 is used instead of NV12 for the inter-element channels because
	// nvvidconv in consumer pipelines cannot use NvBufSurfaceCopy for a
	// format change (I420 ≠ NV12) and is forced to use the VIC correctly.
	i420Caps := fmt.Sprintf(
		"video/x-raw,format=I420,width=%d,height=%d,framerate=%d/1",
		cam.Width, cam.Height, cam.Framerate,
	)

	var source string
	switch strings.ToUpper(cam.Format) {
	case "YUY2", "YUYV":
		// YUY2 is CPU memory — timecodestamper runs first (metadata only, no
		// pixel access), then a single nvvidconv does flip + YUY2→I420 via VIC.
		source = fmt.Sprintf(
			"v4l2src device=%s do-timestamp=true ! "+
				"video/x-raw,width=%d,height=%d,framerate=%d/1 ! "+
				"timecodestamper source=rtc ! "+
				"nvvidconv flip-method=%d ! %s",
			dev, cam.Width, cam.Height, cam.Framerate, flip, i420Caps,
		)
	default: // MJPEG
		// Decoder outputs NVMM NV12. A single nvvidconv does flip +
		// NVMM NV12→CPU I420 in one VIC pass, then timecodestamper adds
		// timecode metadata to the plain CPU I420 buffer.
		source = fmt.Sprintf(
			"v4l2src device=%s do-timestamp=true ! "+
				"image/jpeg,width=%d,height=%d,framerate=%d/1 ! "+
				"nvv4l2decoder mjpeg=true enable-max-performance=true ! "+
				"nvvidconv flip-method=%d ! %s ! "+
				"timecodestamper source=rtc",
			dev, cam.Width, cam.Height, cam.Framerate, flip, i420Caps,
		)
	}

	// Both channels share the same I420 buffer via tee ref-counting — zero
	// extra copy. intervideosink accepts plain video/x-raw (not NVMM).
	return source +
		" ! tee name=t " +
		fmt.Sprintf("t. ! queue max-size-buffers=2 leaky=downstream ! intervideosink channel=%q sync=false ", recCh) +
		fmt.Sprintf("t. ! queue max-size-buffers=2 leaky=downstream ! intervideosink channel=%q sync=false", strCh)
}

// BuildVideoRecordStr builds the recording pipeline for a camera.
// Reads frames from the inter-element record channel and encodes to H.264 fragmented MP4.
func BuildVideoRecordStr(cam config.Camera, outputPath string) string {
	recCh, _ := interChannel("v", cam.Name)
	// intervideosrc delivers video/x-raw,I420 (plain GLib buffer, no
	// NvBufSurface). nvvidconv receives clean I420 and uses VIC to convert
	// to NVMM NV12 without NvBufSurfaceCopy.
	rawCaps := fmt.Sprintf(
		"video/x-raw,width=%d,height=%d,framerate=%d/1,format=I420",
		cam.Width, cam.Height, cam.Framerate,
	)
	return fmt.Sprintf(
		"intervideosrc channel=%q ! %s ! "+
			"nvvidconv ! video/x-raw(memory:NVMM),format=NV12 ! "+
			"nvv4l2h264enc bitrate=%d idrinterval=%d insert-sps-pps=true profile=4 ! "+
			"h264parse ! mp4mux fragment-duration=500 ! filesink location=%s sync=false",
		recCh, rawCaps, videoBitrate(), cam.Framerate/2, outputPath,
	)
}

// BuildVideoStreamStr builds the persistent streaming pipeline for a camera.
// Reads frames from the inter-element stream channel, scales to the configured
// stream resolution, encodes to AV1, and sends via SRT.
// name=avenc: runtime bitrate and IDR control via WatchLocalStats / ForceIDR.
func BuildVideoStreamStr(cam config.Camera, srtPort int) string {
	_, strCh := interChannel("v", cam.Name)
	// intervideosrc delivers video/x-raw,I420 (plain GLib buffer, no
	// NvBufSurface). nvvidconv receives clean I420 and uses VIC to convert
	// to NVMM NV12 (and scale if stream resolution differs from source).
	srcCaps := fmt.Sprintf(
		"video/x-raw,width=%d,height=%d,framerate=%d/1,format=I420",
		cam.Width, cam.Height, cam.Framerate,
	)
	dstCaps := fmt.Sprintf(
		"video/x-raw(memory:NVMM),width=%d,height=%d,framerate=%d/1,format=NV12",
		cam.StreamWidth(), cam.StreamHeight(), cam.StreamFramerate(),
	)

	var enc string
	if intraRefreshPeriod() > 0 {
		enc = fmt.Sprintf(
			"nvv4l2av1enc name=avenc control-rate=1 disable-cdf=false bitrate=%d insert-seq-hdr=true",
			cam.StreamBitrate(),
		)
	} else {
		enc = fmt.Sprintf(
			"nvv4l2av1enc name=avenc control-rate=1 disable-cdf=false bitrate=%d idrinterval=%d insert-seq-hdr=true",
			cam.StreamBitrate(), cam.StreamFramerate()*2,
		)
	}

	// intervideosrc delivers video/x-raw,I420. nvvidconv receives I420 and
	// uses the VIC to convert to NVMM NV12 (format change prevents
	// NvBufSurfaceCopy, forcing the correct VIC code path).
	// wait-for-connection=false: pipeline starts immediately without blocking
	// on the SRT handshake; srtsink drops frames until connected, then sends.
	// av1parse is intentionally absent: nvv4l2av1enc already outputs a valid
	// OBU stream (confirmed by the old direct-WebRTC pipeline that fed the
	// same raw output to pion WriteSample). Removing av1parse eliminates
	// GstBaseParse's format-probing phase (which buffers frames at startup)
	// and ensures srtsink sends one SRT message per encoded frame rather
	// than one message per OBU, which lets the receiver's av1parse output
	// one temporal unit per srt_recvmsg without re-assembly overhead.
	return fmt.Sprintf(
		"intervideosrc channel=%q ! %s ! "+
			"nvvidconv ! %s ! "+
			"%s ! srtsink name=srtsink uri=%q sync=false wait-for-connection=false",
		strCh, srcCaps, dstCaps, enc,
		srtCallerURI(srtPort, cam.Name, "camera"),
	)
}

// ── Audio pipelines ──────────────────────────────────────────────────────────

// BuildAudioSourceStr builds the always-running capture pipeline for a microphone.
// Feeds two inter-element channels: one for recording and one for streaming.
func BuildAudioSourceStr(mic config.Microphone, alsaDev string) string {
	recCh, strCh := interChannel("a", mic.Name)
	caps := fmt.Sprintf("audio/x-raw,format=S16LE,rate=%d,channels=%d", mic.SampleRate, mic.Channels)
	return fmt.Sprintf(
		"alsasrc device=%s do-timestamp=true ! "+
			"audio/x-raw,rate=%d,channels=%d ! audioconvert ! %s ! "+
			"tee name=at "+
			"at. ! queue max-size-buffers=8 leaky=downstream ! interaudiosink channel=%q sync=false "+
			"at. ! queue max-size-buffers=8 leaky=downstream ! interaudiosink channel=%q sync=false",
		alsaDev, mic.SampleRate, mic.Channels, caps, recCh, strCh,
	)
}

// BuildAudioRecordStr builds the recording pipeline for a microphone.
// Reads from the inter-element record channel and muxes AAC audio with a black H.264
// video track (SMPTE timecode) into a fragmented MP4.
func BuildAudioRecordStr(mic config.Microphone, outputPath string) string {
	recCh, _ := interChannel("a", mic.Name)
	caps := fmt.Sprintf("audio/x-raw,format=S16LE,rate=%d,channels=%d", mic.SampleRate, mic.Channels)

	const (
		// blackFPS is deliberately low: this track only exists so ffmpeg-family
		// tools see a video stream alongside the audio, nobody watches it, so
		// there's no reason to spend hardware encoder cycles on 25fps of a
		// static frame.
		blackFPS     = 5
		blackBitrate = 100_000
	)

	blackTrack := fmt.Sprintf(
		"videotestsrc pattern=black is-live=true ! "+
			"video/x-raw,width=320,height=240,framerate=%d/1 ! "+
			"timecodestamper source=rtc ! "+
			"nvvidconv ! video/x-raw(memory:NVMM),format=NV12 ! "+
			"nvv4l2h264enc bitrate=%d idrinterval=%d insert-sps-pps=true profile=4 ! "+
			"h264parse ! mux.",
		blackFPS, blackBitrate, blackFPS/2,
	)

	return fmt.Sprintf(
		"mp4mux name=mux fragment-duration=500 ! filesink location=%s sync=false "+
			"%s "+
			"interaudiosrc channel=%q ! %s ! audioconvert ! "+
			"avenc_aac bitrate=%d ! aacparse ! mux.",
		outputPath, blackTrack, recCh, caps, audioBitrate(),
	)
}

// BuildAudioStreamStr builds the persistent streaming pipeline for a microphone.
// Reads from the inter-element stream channel, encodes to Opus, and sends via SRT.
// If Stream.Channels is set below the capture channel count (e.g. stereo capture, mono
// stream), an extra audioconvert downmixes just before the encoder — the recording path
// (BuildAudioRecordStr) is untouched and keeps the full capture channel count.
func BuildAudioStreamStr(mic config.Microphone, srtPort int) string {
	_, strCh := interChannel("a", mic.Name)
	caps := fmt.Sprintf("audio/x-raw,format=S16LE,rate=%d,channels=%d", mic.SampleRate, mic.Channels)

	downmix := ""
	if ch := mic.StreamChannels(); ch > 0 && ch != mic.Channels {
		downmix = fmt.Sprintf("audioconvert ! audio/x-raw,channels=%d ! ", ch)
	}

	return fmt.Sprintf(
		"interaudiosrc channel=%q ! %s ! "+
			downmix+
			"opusenc bitrate=%d frame-size=20 perfect-timestamp=true ! "+
			"srtsink name=srtsink uri=%q sync=false wait-for-connection=false",
		strCh, caps, mic.StreamBitrate(),
		srtCallerURI(srtPort, mic.Name, "microphone"),
	)
}
