package pipeline

import (
	"strings"
	"testing"

	"racecast-emitter/internal/config"
)

func TestFlipMethod(t *testing.T) {
	tests := []struct {
		vertical, horizontal bool
		want                 int
	}{
		{false, false, 0},
		{false, true, 4},
		{true, false, 6},
		{true, true, 2},
	}
	for _, tt := range tests {
		if got := flipMethod(tt.vertical, tt.horizontal); got != tt.want {
			t.Errorf("flipMethod(%v, %v) = %d, want %d", tt.vertical, tt.horizontal, got, tt.want)
		}
	}
}

func TestSanitize(t *testing.T) {
	if got := sanitize("Front Cam/1"); got != "Front_Cam-1" {
		t.Errorf("sanitize() = %q, want %q", got, "Front_Cam-1")
	}
}

func TestInterChannel(t *testing.T) {
	rec, str := interChannel("v", "Front Cam/1")
	if rec != "rc:vrec:Front_Cam-1" {
		t.Errorf("rec channel = %q, want %q", rec, "rc:vrec:Front_Cam-1")
	}
	if str != "rc:vstr:Front_Cam-1" {
		t.Errorf("str channel = %q, want %q", str, "rc:vstr:Front_Cam-1")
	}
}

func TestStreamKey(t *testing.T) {
	if got := StreamKey("Habitacle", "camera"); got != "Habitacle:camera" {
		t.Errorf("StreamKey(Habitacle, camera) = %q, want %q", got, "Habitacle:camera")
	}
	if got := StreamKey("Habitacle", "microphone"); got != "Habitacle:microphone" {
		t.Errorf("StreamKey(Habitacle, microphone) = %q, want %q", got, "Habitacle:microphone")
	}
	// The whole point of StreamKey: a camera and mic sharing Name (allowed by
	// config.validate()) must not produce the same key.
	if StreamKey("Habitacle", "camera") == StreamKey("Habitacle", "microphone") {
		t.Error("StreamKey must differentiate camera and microphone sharing the same Name")
	}
}

// TestStreamKeyMatchesSrtCallerURI guards against StreamKey and srtCallerURI's
// streamid drifting apart — RaceCast-Receiver's stream_close handling
// (fed by StreamKey, via SendStreamClose) must key by the exact same string
// as the SRT streamid it already parses on connect, or a stream_close signal
// silently fails to match the stream it's meant to close.
func TestStreamKeyMatchesSrtCallerURI(t *testing.T) {
	t.Setenv("RC_SRT_HOST", "1.2.3.4")
	for _, source := range []string{"camera", "microphone"} {
		key := StreamKey("Habitacle", source)
		uri := srtCallerURI(9000, "Habitacle", source)
		if !strings.Contains(uri, "streamid="+key) {
			t.Errorf("srtCallerURI(...,%q) = %q, does not contain streamid=%s (from StreamKey)", source, uri, key)
		}
	}
}

func TestSrtCallerURI(t *testing.T) {
	t.Run("default latency", func(t *testing.T) {
		t.Setenv("RC_SRT_HOST", "1.2.3.4")
		t.Setenv("RC_SRT_LATENCY", "")

		uri := srtCallerURI(9000, "Route", "camera")
		want := "srt://1.2.3.4:9000?streamid=Route:camera&latency=800&mode=caller&iptos=136"
		if uri != want {
			t.Errorf("srtCallerURI() = %q, want %q", uri, want)
		}
	})

	t.Run("custom latency from env", func(t *testing.T) {
		t.Setenv("RC_SRT_HOST", "1.2.3.4")
		t.Setenv("RC_SRT_LATENCY", "1200")

		uri := srtCallerURI(9000, "Route", "camera")
		if !strings.Contains(uri, "latency=1200") {
			t.Errorf("srtCallerURI() = %q, want latency=1200", uri)
		}
	})

	t.Run("streamid differentiates camera and microphone sources", func(t *testing.T) {
		t.Setenv("RC_SRT_HOST", "1.2.3.4")

		camURI := srtCallerURI(9000, "Cockpit", "camera")
		micURI := srtCallerURI(9000, "Cockpit", "microphone")
		if !strings.Contains(camURI, "streamid=Cockpit:camera") {
			t.Errorf("camera uri = %q", camURI)
		}
		if !strings.Contains(micURI, "streamid=Cockpit:microphone") {
			t.Errorf("microphone uri = %q", micURI)
		}
	})
}

func TestBuildVideoSourceStr_FormatBranches(t *testing.T) {
	base := config.Camera{
		Name: "Front", Width: 1920, Height: 1080, Framerate: 30,
		VerticalFlip: true,
	}

	t.Run("MJPEG (default) path uses the hardware JPEG decoder", func(t *testing.T) {
		cam := base
		cam.Format = ""
		got := BuildVideoSourceStr(cam, "/dev/video0")
		if !strings.Contains(got, "image/jpeg,width=1920,height=1080,framerate=30/1") {
			t.Errorf("missing MJPEG caps: %s", got)
		}
		if !strings.Contains(got, "nvv4l2decoder mjpeg=true") {
			t.Errorf("missing MJPEG decoder: %s", got)
		}
		if !strings.Contains(got, "nvvidconv flip-method=6") {
			t.Errorf("missing vertical flip-method=6: %s", got)
		}
	})

	t.Run("YUY2 path skips the JPEG decoder", func(t *testing.T) {
		cam := base
		cam.Format = "YUY2"
		got := BuildVideoSourceStr(cam, "/dev/video0")
		if strings.Contains(got, "nvv4l2decoder") {
			t.Errorf("YUY2 path should not use the JPEG decoder: %s", got)
		}
		if !strings.Contains(got, "video/x-raw,width=1920,height=1080,framerate=30/1") {
			t.Errorf("missing raw caps: %s", got)
		}
		if !strings.Contains(got, "nvvidconv flip-method=6") {
			t.Errorf("missing vertical flip-method=6: %s", got)
		}
	})

	t.Run("both branches tee into the same inter-channel names", func(t *testing.T) {
		cam := base
		got := BuildVideoSourceStr(cam, "/dev/video0")
		recCh, strCh := interChannel("v", cam.Name)
		if !strings.Contains(got, "channel=\""+recCh+"\"") {
			t.Errorf("missing record channel %q: %s", recCh, got)
		}
		if !strings.Contains(got, "channel=\""+strCh+"\"") {
			t.Errorf("missing stream channel %q: %s", strCh, got)
		}
	})
}

func TestBuildVideoSourceStrFake_UsesSnowPatternAndSameChannels(t *testing.T) {
	cam := config.Camera{Name: "Front", Width: 1920, Height: 1080, Framerate: 30}
	got := BuildVideoSourceStrFake(cam)

	if !strings.Contains(got, "videotestsrc pattern=snow") {
		t.Errorf("missing snow test pattern source: %s", got)
	}
	if strings.Contains(got, "v4l2src") {
		t.Errorf("fake source must not touch v4l2src: %s", got)
	}
	if !strings.Contains(got, "video/x-raw,format=I420,width=1920,height=1080,framerate=30/1") {
		t.Errorf("missing matching I420 caps: %s", got)
	}
	// Must tee into the exact same inter-channel names as the real source,
	// so record/stream pipelines can't tell the difference.
	recCh, strCh := interChannel("v", cam.Name)
	if !strings.Contains(got, "channel=\""+recCh+"\"") {
		t.Errorf("missing record channel %q: %s", recCh, got)
	}
	if !strings.Contains(got, "channel=\""+strCh+"\"") {
		t.Errorf("missing stream channel %q: %s", strCh, got)
	}
}

func TestBuildAudioSourceStrFake_UsesWhiteNoiseAndSameChannels(t *testing.T) {
	mic := config.Microphone{Name: "Cockpit", SampleRate: 48000, Channels: 2}
	got := BuildAudioSourceStrFake(mic)

	if !strings.Contains(got, "audiotestsrc wave=white-noise") {
		t.Errorf("missing white-noise test source: %s", got)
	}
	if strings.Contains(got, "alsasrc") {
		t.Errorf("fake source must not touch alsasrc: %s", got)
	}
	if !strings.Contains(got, "audio/x-raw,format=S16LE,rate=48000,channels=2") {
		t.Errorf("missing matching PCM caps: %s", got)
	}
	recCh, strCh := interChannel("a", mic.Name)
	if !strings.Contains(got, "channel=\""+recCh+"\"") {
		t.Errorf("missing record channel %q: %s", recCh, got)
	}
	if !strings.Contains(got, "channel=\""+strCh+"\"") {
		t.Errorf("missing stream channel %q: %s", strCh, got)
	}
}

func TestBuildVideoStreamStr_IntraRefreshBranches(t *testing.T) {
	cam := config.Camera{
		Name: "Front", Width: 1920, Height: 1080, Framerate: 30,
		Stream: &config.StreamConfig{Bitrate: 4_000_000},
	}
	t.Setenv("RC_SRT_HOST", "1.2.3.4")

	t.Run("intra-refresh disabled falls back to idrinterval", func(t *testing.T) {
		t.Setenv("RC_VIDEO_INTRA_REFRESH", "0")
		got := BuildVideoStreamStr(cam, 9000)
		if !strings.Contains(got, "idrinterval=60") { // framerate*2
			t.Errorf("missing idrinterval fallback: %s", got)
		}
	})

	t.Run("intra-refresh enabled omits idrinterval", func(t *testing.T) {
		t.Setenv("RC_VIDEO_INTRA_REFRESH", "30")
		got := BuildVideoStreamStr(cam, 9000)
		if strings.Contains(got, "idrinterval") {
			t.Errorf("idrinterval should be absent when intra-refresh is enabled: %s", got)
		}
	})

	t.Run("encoder is named avenc for runtime control and uses configured bitrate", func(t *testing.T) {
		t.Setenv("RC_VIDEO_INTRA_REFRESH", "0")
		got := BuildVideoStreamStr(cam, 9000)
		if !strings.Contains(got, "nvv4l2av1enc name=avenc") {
			t.Errorf("missing named encoder: %s", got)
		}
		if !strings.Contains(got, "bitrate=4000000") {
			t.Errorf("missing configured bitrate: %s", got)
		}
	})

	t.Run("sink is named srtsink for runtime stats/control", func(t *testing.T) {
		got := BuildVideoStreamStr(cam, 9000)
		if !strings.Contains(got, "srtsink name=srtsink") {
			t.Errorf("missing named sink: %s", got)
		}
	})
}

func TestBuildVideoRecordStr_StampsTimecodeBeforeNvvidconv(t *testing.T) {
	cam := config.Camera{Name: "Front", Width: 1920, Height: 1080, Framerate: 30}
	got := BuildVideoRecordStr(cam, "records/out.mov")

	if !strings.Contains(got, "timecodestamper source=rtc") {
		t.Fatalf("missing timecodestamper: %s", got)
	}
	// timecodestamper only accepts raw video/x-raw caps, so it must appear
	// between intervideosrc and this pipeline's own nvvidconv (which
	// converts to NVMM) — not after, where the muxer would need it but the
	// caps would already have moved past what it can process.
	srcIdx := strings.Index(got, "intervideosrc")
	tcIdx := strings.Index(got, "timecodestamper")
	nvvidconvIdx := strings.Index(got, "nvvidconv")
	if !(srcIdx < tcIdx && tcIdx < nvvidconvIdx) {
		t.Errorf("expected intervideosrc < timecodestamper < nvvidconv, got indices %d, %d, %d: %s",
			srcIdx, tcIdx, nvvidconvIdx, got)
	}
	// Verified empirically (see build.go's comment): mp4mux silently drops
	// the timecode track qtmux writes from the same metadata — regressing
	// this to mp4mux would compile and run fine while quietly breaking
	// Resolve sync, so pin the muxer explicitly rather than only asserting
	// that *a* muxer is present.
	if !strings.Contains(got, "qtmux") {
		t.Errorf("expected qtmux (not mp4mux, which drops the tmcd track): %s", got)
	}
	if strings.Contains(got, "mp4mux") {
		t.Errorf("mp4mux is known to silently drop the timecode track, must not be used: %s", got)
	}
}

func TestBuildVideoSourceStr_DoesNotStampTimecode(t *testing.T) {
	// Timecoding now happens only in BuildVideoRecordStr (the sole consumer
	// that needs it) — the shared source pipeline stamping it too would just
	// add uncertain hops (this pipeline's own conversion, the tee, the
	// inter-element handoff) for no benefit. See BuildVideoRecordStr.
	cam := config.Camera{Name: "Front", Width: 1920, Height: 1080, Framerate: 30}
	for _, format := range []string{"", "YUY2"} {
		cam.Format = format
		got := BuildVideoSourceStr(cam, "/dev/video0")
		if strings.Contains(got, "timecodestamper") {
			t.Errorf("format %q: unexpected timecodestamper in source pipeline: %s", format, got)
		}
	}
}

func TestBuildAudioRecordStr_WritesPlainWAV(t *testing.T) {
	mic := config.Microphone{Name: "Cockpit", SampleRate: 48000, Channels: 1}
	got := BuildAudioRecordStr(mic, "records/out.wav")

	if !strings.Contains(got, "wavenc") {
		t.Errorf("missing wavenc: %s", got)
	}
	if !strings.Contains(got, "filesink location=records/out.wav") {
		t.Errorf("missing output path: %s", got)
	}
	if !strings.Contains(got, "audio/x-raw,format=S16LE,rate=48000,channels=1") {
		t.Errorf("missing raw PCM caps: %s", got)
	}
	// The black-video-track/AAC/mp4mux workaround this replaced (see bwf.go)
	// must be gone entirely, not just unused.
	for _, gone := range []string{"videotestsrc", "avenc_aac", "mp4mux", "timecodestamper"} {
		if strings.Contains(got, gone) {
			t.Errorf("unexpected leftover %q from the old black-video-track workaround: %s", gone, got)
		}
	}
}

func TestBuildAudioStreamStr_UsesOpusAndConfiguredBitrate(t *testing.T) {
	t.Setenv("RC_SRT_HOST", "1.2.3.4")
	mic := config.Microphone{
		Name: "Cockpit", SampleRate: 48000, Channels: 1,
		Stream: &config.StreamConfig{Bitrate: 96_000},
	}
	got := BuildAudioStreamStr(mic, 9000)
	if !strings.Contains(got, "opusenc name=aenc bitrate=96000") {
		t.Errorf("missing named encoder with configured Opus bitrate: %s", got)
	}
	if !strings.Contains(got, "streamid=Cockpit:microphone") {
		t.Errorf("missing microphone streamid: %s", got)
	}
}

func TestBuildAudioStreamStr_DownmixesToMonoWhenConfigured(t *testing.T) {
	t.Setenv("RC_SRT_HOST", "1.2.3.4")
	mic := config.Microphone{
		Name: "Habitacle", SampleRate: 48000, Channels: 2,
		Stream: &config.StreamConfig{Bitrate: 24_000, Channels: 1},
	}
	got := BuildAudioStreamStr(mic, 9000)
	if !strings.Contains(got, "audioconvert ! audio/x-raw,channels=1 ! opusenc") {
		t.Errorf("missing mono downmix ahead of the encoder: %s", got)
	}

	// Recording must be unaffected: still full capture channel count, no downmix.
	rec := BuildAudioRecordStr(mic, "records/out.wav")
	if strings.Contains(rec, "channels=1") {
		t.Errorf("recording path should keep full stereo capture, got: %s", rec)
	}
}

func TestBuildAudioStreamStr_NoDownmixWhenChannelsMatch(t *testing.T) {
	t.Setenv("RC_SRT_HOST", "1.2.3.4")
	mic := config.Microphone{
		Name: "Cockpit", SampleRate: 48000, Channels: 1,
		Stream: &config.StreamConfig{Bitrate: 96_000},
	}
	got := BuildAudioStreamStr(mic, 9000)
	if strings.Contains(got, "audioconvert") {
		t.Errorf("should not insert a downmix stage when stream channels == capture channels: %s", got)
	}
}
