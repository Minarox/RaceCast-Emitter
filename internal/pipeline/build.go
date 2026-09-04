package pipeline

import (
	"fmt"
	"os"
	"strings"

	"racecast-emitter/internal/config"
)

// resolutionCapsfilterName names the capsfilter BuildVideoStreamStr inserts
// between nvvidconv and the encoder. bandwidth.go's resolution-tier switching
// targets it by name via GstPipeline.SetResolution to change the encoder's
// input resolution live, without touching srtsink or the SRT connection.
const resolutionCapsfilterName = "rescap"

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

// StreamKey builds the "name:source" identity RaceCast-Receiver uses to key
// its per-stream state — see srtCallerURI's streamid below, which this must
// stay identical to. Exported so callers outside this package (main.go's
// stream_close notifications) that need to name a stream to the receiver
// without also connecting can build the same identity: a camera and a
// microphone may share Name (validate() allows it, e.g. one device's video
// and audio interfaces), so name alone can't identify which one closed.
func StreamKey(name, source string) string {
	return name + ":" + source
}

// srtCallerURI builds an SRT caller URI; streamid = "name:source" (e.g. "Route:camera").
func srtCallerURI(port int, name, source string) string {
	host := os.Getenv("RC_SRT_HOST")
	latency := envInt("RC_SRT_LATENCY", 800)
	streamID := StreamKey(name, source)
	// iptos=136 = DSCP AF41: marks packets as video streaming for router QoS.
	// Unencrypted: this SRT traffic runs inside a WireGuard tunnel to the
	// receiver, so an SRT-layer passphrase would just double-encrypt it.
	return fmt.Sprintf("srt://%s:%d?streamid=%s&latency=%d&mode=caller&iptos=136", host, port, streamID, latency)
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

	// Timecoding is NOT done here. timecodestamper only ever attaches a
	// GstVideoTimeCodeMeta on a raw-video buffer, and that meta still has to
	// cross this pipeline's own recompression hops downstream (nvvidconv's
	// NVMM conversion, the hardware encoder, the parser) before it could
	// reach any muxer — stamping in the shared source pipeline would add
	// three more uncertain hops on top of that (this decode/convert step,
	// the tee, and the inter-element handoff) for no benefit, since nothing
	// here consumes the timecode itself. See BuildVideoRecordStr, the only
	// consumer that actually needs it, for where it's stamped instead.
	var source string
	switch strings.ToUpper(cam.Format) {
	case "YUY2", "YUYV":
		source = fmt.Sprintf(
			"v4l2src device=%s do-timestamp=true ! "+
				"video/x-raw,width=%d,height=%d,framerate=%d/1 ! "+
				"nvvidconv flip-method=%d ! %s",
			dev, cam.Width, cam.Height, cam.Framerate, flip, i420Caps,
		)
	default: // MJPEG
		// Decoder outputs NVMM NV12; nvvidconv does flip + NVMM NV12→CPU I420
		// in one VIC pass.
		source = fmt.Sprintf(
			"v4l2src device=%s do-timestamp=true ! "+
				"image/jpeg,width=%d,height=%d,framerate=%d/1 ! "+
				"nvv4l2decoder mjpeg=true enable-max-performance=true ! "+
				"nvvidconv flip-method=%d ! %s",
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

// BuildVideoSourceStrFake builds the always-running capture pipeline for a
// camera in --fake-devices mode: a synthetic "snow" test pattern (real
// pseudo-random pixel data, not a static image) in place of a v4l2 capture,
// so the record/stream/SRT path can be exercised end-to-end without a
// physical camera attached. Feeds the same two inter-element channels as
// BuildVideoSourceStr, so nothing downstream (record, stream, ABR, the
// bandwidth coordinator) needs to know the difference.
func BuildVideoSourceStrFake(cam config.Camera) string {
	recCh, strCh := interChannel("v", cam.Name)
	i420Caps := fmt.Sprintf(
		"video/x-raw,format=I420,width=%d,height=%d,framerate=%d/1",
		cam.Width, cam.Height, cam.Framerate,
	)
	source := fmt.Sprintf("videotestsrc pattern=snow is-live=true ! %s", i420Caps)
	return source +
		" ! tee name=t " +
		fmt.Sprintf("t. ! queue max-size-buffers=2 leaky=downstream ! intervideosink channel=%q sync=false ", recCh) +
		fmt.Sprintf("t. ! queue max-size-buffers=2 leaky=downstream ! intervideosink channel=%q sync=false", strCh)
}

// BuildVideoRecordStr builds the recording pipeline for a camera.
// Reads frames from the inter-element record channel and encodes to H.264 in
// a fragmented QuickTime (.mov) file, carrying an embedded SMPTE timecode
// track for DaVinci Resolve auto-sync.
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
		// timecodestamper must run here, in the raw-video domain right after
		// intervideosrc — it only accepts video/x-raw caps — before this
		// pipeline's own nvvidconv/NVMM conversion, hardware encoder, and
		// parser.
		//
		// Verified empirically on this hardware (GStreamer 1.24.2, R39):
		// GstVideoTimeCodeMeta survives nvvidconv → nvv4l2h264enc →
		// h264parse intact — the muxer reads a valid timecode from the
		// first buffer it receives either way. The part that actually
		// mattered, and was wrong until this was tested, is the muxer
		// itself: qtmux writes a dedicated tmcd timecode track from that
		// metadata, but mp4mux — despite sharing gst-plugins-good's isomp4
		// code with qtmux — silently drops it and produces a video-only
		// file. Do not swap this back to mp4mux without re-verifying.
		"intervideosrc channel=%q ! %s ! "+
			"timecodestamper source=rtc ! "+
			"nvvidconv ! video/x-raw(memory:NVMM),format=NV12 ! "+
			"nvv4l2h264enc bitrate=%d idrinterval=%d insert-sps-pps=true profile=4 ! "+
			"h264parse ! qtmux fragment-duration=500 ! filesink location=%s sync=false",
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
			"nvvidconv ! capsfilter name=%s caps=%q ! "+
			"%s ! srtsink name=srtsink uri=%q sync=false wait-for-connection=false",
		strCh, srcCaps, resolutionCapsfilterName, dstCaps, enc,
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

// BuildAudioSourceStrFake builds the always-running capture pipeline for a
// microphone in --fake-devices mode: real white-noise samples
// (audiotestsrc wave=white-noise, not silence) in place of an ALSA capture,
// so the audio record/stream/SRT path can be exercised without a physical
// microphone attached. Feeds the same two inter-element channels as
// BuildAudioSourceStr.
func BuildAudioSourceStrFake(mic config.Microphone) string {
	recCh, strCh := interChannel("a", mic.Name)
	caps := fmt.Sprintf("audio/x-raw,format=S16LE,rate=%d,channels=%d", mic.SampleRate, mic.Channels)
	return fmt.Sprintf(
		"audiotestsrc wave=white-noise is-live=true ! %s ! "+
			"tee name=at "+
			"at. ! queue max-size-buffers=8 leaky=downstream ! interaudiosink channel=%q sync=false "+
			"at. ! queue max-size-buffers=8 leaky=downstream ! interaudiosink channel=%q sync=false",
		caps, recCh, strCh,
	)
}

// BuildAudioRecordStr builds the recording pipeline for a microphone.
// Reads from the inter-element record channel and writes plain PCM to a WAV
// file via wavenc — uncompressed, since BWF (see bwf.go) is fundamentally a
// PCM format and Resolve's timecode auto-sync needs sample-accurate data
// anyway. No black-video-track workaround: injectBWFTimeReference (called
// once this pipeline has fully stopped — see Poll's onStopped) patches a
// proper "bext" chunk into the file afterwards, carrying the same kind of
// start-time reference a professional field recorder would embed natively,
// which Resolve reads directly without needing a synthetic video track at all.
func BuildAudioRecordStr(mic config.Microphone, outputPath string) string {
	recCh, _ := interChannel("a", mic.Name)
	caps := fmt.Sprintf("audio/x-raw,format=S16LE,rate=%d,channels=%d", mic.SampleRate, mic.Channels)
	return fmt.Sprintf(
		"interaudiosrc channel=%q ! %s ! audioconvert ! "+
			"wavenc ! filesink location=%s sync=false",
		recCh, caps, outputPath,
	)
}

// BuildAudioStreamStr builds the persistent streaming pipeline for a microphone.
// Reads from the inter-element stream channel, encodes to Opus, and sends via SRT.
// If Stream.Channels is set below the capture channel count (e.g. stereo capture, mono
// stream), an extra audioconvert downmixes just before the encoder — the recording path
// (BuildAudioRecordStr) is untouched and keeps the full capture channel count.
// name=aenc: runtime bitrate control via WatchLocalStats, same mechanism as the
// video path's "avenc" (see feedback.go) — opusenc has no "force-IDR" signal, so
// the ForceIDR call WatchLocalStats makes on SRT (re)connect is a harmless no-op
// on this element (force_idr in gst.go checks the signal exists before emitting).
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
			"opusenc name=aenc bitrate=%d frame-size=20 perfect-timestamp=true ! "+
			"srtsink name=srtsink uri=%q sync=false wait-for-connection=false",
		strCh, caps, mic.StreamBitrate(),
		srtCallerURI(srtPort, mic.Name, "microphone"),
	)
}
