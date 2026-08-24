package pipeline

import (
	"os"
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

func TestSrtCallerURI(t *testing.T) {
	t.Run("without passphrase, default latency", func(t *testing.T) {
		t.Setenv("RC_SRT_HOST", "1.2.3.4")
		t.Setenv("RC_SRT_PASSPHRASE", "")
		t.Setenv("RC_SRT_LATENCY", "")

		uri := srtCallerURI(9000, "Route", "camera")
		want := "srt://1.2.3.4:9000?streamid=Route:camera&latency=800&mode=caller&iptos=136"
		if uri != want {
			t.Errorf("srtCallerURI() = %q, want %q", uri, want)
		}
	})

	t.Run("with passphrase appends escaped passphrase and pbkeylen", func(t *testing.T) {
		t.Setenv("RC_SRT_HOST", "1.2.3.4")
		t.Setenv("RC_SRT_PASSPHRASE", "a b&c")

		uri := srtCallerURI(9000, "Route", "camera")
		if !strings.Contains(uri, "passphrase=a+b%26c") {
			t.Errorf("srtCallerURI() = %q, missing escaped passphrase", uri)
		}
		if !strings.HasSuffix(uri, "&pbkeylen=32") {
			t.Errorf("srtCallerURI() = %q, missing pbkeylen suffix", uri)
		}
	})

	t.Run("custom latency from env", func(t *testing.T) {
		t.Setenv("RC_SRT_HOST", "1.2.3.4")
		t.Setenv("RC_SRT_PASSPHRASE", "")
		t.Setenv("RC_SRT_LATENCY", "1200")

		uri := srtCallerURI(9000, "Route", "camera")
		if !strings.Contains(uri, "latency=1200") {
			t.Errorf("srtCallerURI() = %q, want latency=1200", uri)
		}
	})

	t.Run("streamid differentiates camera and microphone sources", func(t *testing.T) {
		t.Setenv("RC_SRT_HOST", "1.2.3.4")
		t.Setenv("RC_SRT_PASSPHRASE", "")

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

func TestBuildVideoStreamStr_IntraRefreshBranches(t *testing.T) {
	cam := config.Camera{
		Name: "Front", Width: 1920, Height: 1080, Framerate: 30,
		Stream: &config.StreamConfig{Bitrate: 4_000_000},
	}
	t.Setenv("RC_SRT_HOST", "1.2.3.4")
	t.Setenv("RC_SRT_PASSPHRASE", "")

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

func TestBuildAudioRecordStr_MuxesBlackVideoTrack(t *testing.T) {
	os.Unsetenv("RC_AUDIO_BITRATE")
	mic := config.Microphone{Name: "Cockpit", SampleRate: 48000, Channels: 1}
	got := BuildAudioRecordStr(mic, "records/out.mp4")

	if !strings.Contains(got, "mp4mux name=mux") {
		t.Errorf("missing named muxer: %s", got)
	}
	if !strings.Contains(got, "filesink location=records/out.mp4") {
		t.Errorf("missing output path: %s", got)
	}
	if !strings.Contains(got, "videotestsrc pattern=black") {
		t.Errorf("missing synthetic black video track: %s", got)
	}
	if !strings.Contains(got, "avenc_aac bitrate=192000") {
		t.Errorf("missing default AAC bitrate: %s", got)
	}
	if strings.Count(got, "mux.") != 2 {
		t.Errorf("expected both the black track and audio to link into mux., got: %s", got)
	}
}

func TestBuildAudioStreamStr_UsesOpusAndConfiguredBitrate(t *testing.T) {
	t.Setenv("RC_SRT_HOST", "1.2.3.4")
	t.Setenv("RC_SRT_PASSPHRASE", "")
	mic := config.Microphone{
		Name: "Cockpit", SampleRate: 48000, Channels: 1,
		Stream: &config.StreamConfig{Bitrate: 96_000},
	}
	got := BuildAudioStreamStr(mic, 9000)
	if !strings.Contains(got, "opusenc bitrate=96000") {
		t.Errorf("missing configured Opus bitrate: %s", got)
	}
	if !strings.Contains(got, "streamid=Cockpit:microphone") {
		t.Errorf("missing microphone streamid: %s", got)
	}
}
