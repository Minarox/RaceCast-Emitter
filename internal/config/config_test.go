package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoad(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "devices.yaml")
	yamlContent := `
cameras:
  - uid: cam-uid-1
    name: Front
    width: 1920
    height: 1080
    framerate: 30
    stream:
      bitrate: 4000000
  - uid: cam-uid-2
    name: Rear
    disabled: true
microphones:
  - uid: mic-uid-1
    name: Cockpit
    sample_rate: 48000
    channels: 1
`
	if err := os.WriteFile(path, []byte(yamlContent), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if len(cfg.Cameras) != 2 {
		t.Fatalf("len(Cameras) = %d, want 2", len(cfg.Cameras))
	}

	front := cfg.Cameras[0]
	if front.Name != "Front" || front.UID != "cam-uid-1" {
		t.Errorf("unexpected camera: %+v", front)
	}
	if !front.HasStream() {
		t.Error("Front.HasStream() = false, want true")
	}
	if front.StreamBitrate() != 4_000_000 {
		t.Errorf("Front.StreamBitrate() = %d, want 4000000", front.StreamBitrate())
	}

	rear := cfg.Cameras[1]
	if !rear.Disabled {
		t.Error("Rear.Disabled = false, want true")
	}
	if rear.HasStream() {
		t.Error("Rear.HasStream() = true, want false (no stream block)")
	}

	if len(cfg.Microphones) != 1 {
		t.Fatalf("len(Microphones) = %d, want 1", len(cfg.Microphones))
	}
	mic := cfg.Microphones[0]
	if mic.Name != "Cockpit" || mic.SampleRate != 48000 || mic.Channels != 1 {
		t.Errorf("unexpected microphone: %+v", mic)
	}
}

func TestLoad_MissingFile(t *testing.T) {
	if _, err := Load("/nonexistent/devices.yaml"); err == nil {
		t.Error("Load() error = nil, want error for missing file")
	}
}

func TestLoad_DuplicateCameraName(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "devices.yaml")
	yamlContent := `
cameras:
  - uid: cam-uid-1
    name: Front
    width: 1920
    height: 1080
    framerate: 30
  - uid: cam-uid-2
    name: Front
    width: 1920
    height: 1080
    framerate: 30
`
	if err := os.WriteFile(path, []byte(yamlContent), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err == nil {
		t.Error("Load() error = nil, want error for duplicate camera name")
	}
}

func TestLoad_DuplicateCameraUID(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "devices.yaml")
	yamlContent := `
cameras:
  - uid: cam-uid-1
    name: Front
    width: 1920
    height: 1080
    framerate: 30
  - uid: cam-uid-1
    name: Rear
    width: 1920
    height: 1080
    framerate: 30
`
	if err := os.WriteFile(path, []byte(yamlContent), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err == nil {
		t.Error("Load() error = nil, want error for duplicate camera uid")
	}
}

func TestLoad_MultipleMainCamerasRejected(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "devices.yaml")
	yamlContent := `
cameras:
  - uid: cam-uid-1
    name: Front
    width: 1920
    height: 1080
    framerate: 30
    main: true
  - uid: cam-uid-2
    name: Rear
    width: 1920
    height: 1080
    framerate: 30
    main: true
`
	if err := os.WriteFile(path, []byte(yamlContent), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err == nil {
		t.Error("Load() error = nil, want error for two cameras marked main")
	}
}

func TestLoad_SingleMainCameraAccepted(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "devices.yaml")
	yamlContent := `
cameras:
  - uid: cam-uid-1
    name: Front
    width: 1920
    height: 1080
    framerate: 30
    main: true
  - uid: cam-uid-2
    name: Rear
    width: 1920
    height: 1080
    framerate: 30
`
	if err := os.WriteFile(path, []byte(yamlContent), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load() error = %v, want nil", err)
	}
	if !cfg.Cameras[0].Main {
		t.Error("Cameras[0].Main = false, want true")
	}
	if cfg.Cameras[1].Main {
		t.Error("Cameras[1].Main = true, want false")
	}
}

func TestLoad_MissingCameraUIDOrName(t *testing.T) {
	dir := t.TempDir()
	for _, tc := range []struct {
		name    string
		content string
	}{
		{"missing uid", "cameras:\n  - name: Front\n    width: 1920\n    height: 1080\n    framerate: 30\n"},
		{"missing name", "cameras:\n  - uid: cam-uid-1\n    width: 1920\n    height: 1080\n    framerate: 30\n"},
	} {
		path := filepath.Join(dir, tc.name+".yaml")
		if err := os.WriteFile(path, []byte(tc.content), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := Load(path); err == nil {
			t.Errorf("%s: Load() error = nil, want error", tc.name)
		}
	}
}

func TestLoad_NameWithSRTUnsafeCharsRejected(t *testing.T) {
	dir := t.TempDir()
	for _, tc := range []struct {
		name    string
		content string
	}{
		{"camera-space", "cameras:\n  - uid: cam-1\n    name: \"Front Camera\"\n    width: 1920\n    height: 1080\n    framerate: 30\n"},
		{"camera-ampersand", "cameras:\n  - uid: cam-1\n    name: \"Front & Rear\"\n    width: 1920\n    height: 1080\n    framerate: 30\n"},
		{"microphone-hash", "microphones:\n  - uid: mic-1\n    name: \"Driver #1\"\n    sample_rate: 48000\n    channels: 1\n"},
	} {
		path := filepath.Join(dir, tc.name+".yaml")
		if err := os.WriteFile(path, []byte(tc.content), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := Load(path); err == nil {
			t.Errorf("%s: Load() error = nil, want error", tc.name)
		}
	}
}

func TestLoad_CameraAndMicrophoneMaySharSameUIDAndName(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "devices.yaml")
	yamlContent := `
cameras:
  - uid: shared-uid
    name: Habitacle
    width: 1920
    height: 1080
    framerate: 30
microphones:
  - uid: shared-uid
    name: Habitacle
    sample_rate: 48000
    channels: 2
`
	if err := os.WriteFile(path, []byte(yamlContent), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err != nil {
		t.Errorf("Load() error = %v, want nil (uid/name reuse across cameras/microphones is fine)", err)
	}
}

func TestLoad_DuplicateMicrophoneNameOrUID(t *testing.T) {
	dir := t.TempDir()
	for _, tc := range []struct {
		name    string
		content string
	}{
		{"duplicate name", "microphones:\n  - uid: mic-1\n    name: Cockpit\n    sample_rate: 48000\n    channels: 1\n  - uid: mic-2\n    name: Cockpit\n    sample_rate: 48000\n    channels: 1\n"},
		{"duplicate uid", "microphones:\n  - uid: mic-1\n    name: Cockpit\n    sample_rate: 48000\n    channels: 1\n  - uid: mic-1\n    name: Codriver\n    sample_rate: 48000\n    channels: 1\n"},
	} {
		path := filepath.Join(dir, tc.name+".yaml")
		if err := os.WriteFile(path, []byte(tc.content), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := Load(path); err == nil {
			t.Errorf("%s: Load() error = nil, want error", tc.name)
		}
	}
}

func TestLoad_EnabledCameraRequiresDimensions(t *testing.T) {
	dir := t.TempDir()
	for _, tc := range []struct {
		name    string
		content string
	}{
		{"zero-width", "cameras:\n  - uid: cam-1\n    name: Front\n    width: 0\n    height: 1080\n    framerate: 30\n"},
		{"zero-height", "cameras:\n  - uid: cam-1\n    name: Front\n    width: 1920\n    height: 0\n    framerate: 30\n"},
		{"zero-framerate", "cameras:\n  - uid: cam-1\n    name: Front\n    width: 1920\n    height: 1080\n    framerate: 0\n"},
	} {
		path := filepath.Join(dir, tc.name+".yaml")
		if err := os.WriteFile(path, []byte(tc.content), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := Load(path); err == nil {
			t.Errorf("%s: Load() error = nil, want error", tc.name)
		}
	}
}

func TestLoad_DisabledCameraSkipsDimensionCheck(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "devices.yaml")
	yamlContent := "cameras:\n  - uid: cam-1\n    name: Spare\n    disabled: true\n"
	if err := os.WriteFile(path, []byte(yamlContent), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err != nil {
		t.Errorf("Load() error = %v, want nil (disabled camera needs no dimensions)", err)
	}
}

func TestLoad_EnabledMicrophoneRequiresSampleRateAndChannels(t *testing.T) {
	dir := t.TempDir()
	for _, tc := range []struct {
		name    string
		content string
	}{
		{"zero-sample-rate", "microphones:\n  - uid: mic-1\n    name: Cockpit\n    sample_rate: 0\n    channels: 1\n"},
		{"zero-channels", "microphones:\n  - uid: mic-1\n    name: Cockpit\n    sample_rate: 48000\n    channels: 0\n"},
	} {
		path := filepath.Join(dir, tc.name+".yaml")
		if err := os.WriteFile(path, []byte(tc.content), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := Load(path); err == nil {
			t.Errorf("%s: Load() error = nil, want error", tc.name)
		}
	}
}

func TestLoad_DisabledMicrophoneSkipsCaptureParamCheck(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "devices.yaml")
	yamlContent := "microphones:\n  - uid: mic-1\n    name: Spare\n    disabled: true\n"
	if err := os.WriteFile(path, []byte(yamlContent), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err != nil {
		t.Errorf("Load() error = %v, want nil (disabled microphone needs no capture params)", err)
	}
}

func TestLoad_NegativeCameraStreamFieldsRejected(t *testing.T) {
	dir := t.TempDir()
	base := "cameras:\n  - uid: cam-1\n    name: Front\n    width: 1920\n    height: 1080\n    framerate: 30\n    stream:\n"
	for _, tc := range []struct {
		name    string
		content string
	}{
		{"negative-width", base + "      width: -1\n      bitrate: 4000000\n"},
		{"negative-height", base + "      height: -1\n      bitrate: 4000000\n"},
		{"negative-framerate", base + "      framerate: -1\n      bitrate: 4000000\n"},
		{"negative-bitrate", base + "      bitrate: -1\n"},
	} {
		path := filepath.Join(dir, tc.name+".yaml")
		if err := os.WriteFile(path, []byte(tc.content), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := Load(path); err == nil {
			t.Errorf("%s: Load() error = nil, want error", tc.name)
		}
	}
}

func TestLoad_NegativeMicrophoneStreamFieldsRejected(t *testing.T) {
	dir := t.TempDir()
	base := "microphones:\n  - uid: mic-1\n    name: Cockpit\n    sample_rate: 48000\n    channels: 2\n    stream:\n"
	for _, tc := range []struct {
		name    string
		content string
	}{
		{"negative-bitrate", base + "      bitrate: -1\n"},
		{"negative-channels", base + "      bitrate: 128000\n      channels: -1\n"},
	} {
		path := filepath.Join(dir, tc.name+".yaml")
		if err := os.WriteFile(path, []byte(tc.content), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := Load(path); err == nil {
			t.Errorf("%s: Load() error = nil, want error", tc.name)
		}
	}
}

func TestLoad_InvalidYAML(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "devices.yaml")
	if err := os.WriteFile(path, []byte("cameras: [this is not: valid: yaml"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err == nil {
		t.Error("Load() error = nil, want error for invalid YAML")
	}
}

func TestCamera_StreamFallbacks(t *testing.T) {
	cam := Camera{Width: 1920, Height: 1080, Framerate: 30}

	if cam.HasStream() {
		t.Error("HasStream() with nil Stream = true, want false")
	}
	if got := cam.StreamWidth(); got != 1920 {
		t.Errorf("StreamWidth() with no override = %d, want capture value 1920", got)
	}
	if got := cam.StreamHeight(); got != 1080 {
		t.Errorf("StreamHeight() with no override = %d, want capture value 1080", got)
	}
	if got := cam.StreamFramerate(); got != 30 {
		t.Errorf("StreamFramerate() with no override = %d, want capture value 30", got)
	}
	if got := cam.StreamBitrate(); got != 0 {
		t.Errorf("StreamBitrate() with nil Stream = %d, want 0", got)
	}

	cam.Stream = &StreamConfig{Width: 1280, Bitrate: 2_000_000}
	if got := cam.StreamWidth(); got != 1280 {
		t.Errorf("StreamWidth() override = %d, want 1280", got)
	}
	if got := cam.StreamHeight(); got != 1080 {
		t.Errorf("StreamHeight() with unset override = %d, want fallback to capture value 1080", got)
	}
	if got := cam.StreamBitrate(); got != 2_000_000 {
		t.Errorf("StreamBitrate() = %d, want 2000000", got)
	}
	if !cam.HasStream() {
		t.Error("HasStream() with positive bitrate = false, want true")
	}

	cam.Stream.Bitrate = 0
	if cam.HasStream() {
		t.Error("HasStream() with zero bitrate = true, want false")
	}
}

func TestMicrophone_StreamFallbacks(t *testing.T) {
	mic := Microphone{}
	if mic.HasStream() {
		t.Error("HasStream() with nil Stream = true, want false")
	}
	if mic.StreamBitrate() != 0 {
		t.Errorf("StreamBitrate() with nil Stream = %d, want 0", mic.StreamBitrate())
	}

	mic.Stream = &StreamConfig{Bitrate: 0}
	if mic.HasStream() {
		t.Error("HasStream() with zero bitrate = true, want false")
	}

	mic.Stream.Bitrate = 128_000
	if !mic.HasStream() {
		t.Error("HasStream() with positive bitrate = false, want true")
	}
	if mic.StreamBitrate() != 128_000 {
		t.Errorf("StreamBitrate() = %d, want 128000", mic.StreamBitrate())
	}
}

func TestMicrophone_StreamChannels(t *testing.T) {
	mic := Microphone{Channels: 2}
	if got := mic.StreamChannels(); got != 2 {
		t.Errorf("StreamChannels() with nil Stream = %d, want 2 (capture fallback)", got)
	}

	mic.Stream = &StreamConfig{Bitrate: 24_000}
	if got := mic.StreamChannels(); got != 2 {
		t.Errorf("StreamChannels() with unset Stream.Channels = %d, want 2 (capture fallback)", got)
	}

	mic.Stream.Channels = 1
	if got := mic.StreamChannels(); got != 1 {
		t.Errorf("StreamChannels() = %d, want 1 (downmix override)", got)
	}
}
