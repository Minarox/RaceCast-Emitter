package utils

import (
	"os"

	"gopkg.in/yaml.v3"
)

// CameraStreamCfg holds streaming-specific settings for a camera.
// Each field overrides the corresponding capture-level value when non-zero.
// This allows streaming at a different resolution, framerate or bitrate than
// the recording pipeline without changing the capture parameters.
type CameraStreamCfg struct {
	// Width overrides the capture width for the stream encoder.
	Width int `yaml:"width"`

	// Height overrides the capture height for the stream encoder.
	Height int `yaml:"height"`

	// Framerate overrides the capture framerate for the stream encoder.
	Framerate int `yaml:"framerate"`

	// Bitrate sets the video encoding bitrate in bits/s (e.g. 2000000 for 2 Mbit/s).
	// Required: the stream will not start if this value is absent or zero.
	Bitrate int `yaml:"bitrate"`
}

// CameraEntry represents a single camera declaration in devices.yaml.
type CameraEntry struct {
	// UID is the udev ID_SERIAL of the USB device (e.g.
	// "Generic_USB_Camera_200901010001"). It is stable across reboots and
	// unaffected by the /dev/videoN enumeration order.
	UID string `yaml:"uid"`

	// Name overrides the track name published to LiveKit.  When empty the
	// V4L2 card name is used.
	Name string `yaml:"name"`

	// Width, Height and Framerate define the capture (and recording) resolution.
	Width     int `yaml:"width"`
	Height    int `yaml:"height"`
	Framerate int `yaml:"framerate"`

	// VerticalFlip mirrors the image top-to-bottom.
	VerticalFlip bool `yaml:"vertical_flip"`

	// HorizontalFlip mirrors the image left-to-right.
	HorizontalFlip bool `yaml:"horizontal_flip"`

	// Disabled prevents this camera from being started.
	// When absent or false the camera is enabled by default.
	Disabled bool `yaml:"disabled"`

	// Stream holds streaming-specific overrides (resolution, fps, bitrate).
	// Each field falls back to the capture-level value when zero.
	Stream CameraStreamCfg `yaml:"stream"`
}

// MicrophoneStreamCfg holds streaming-specific settings for a microphone.
type MicrophoneStreamCfg struct {
	// Bitrate sets the Opus encoding bitrate in bits/s (e.g. 96000 for 96 kbit/s).
	// Required: the stream will not start if this value is absent or zero.
	Bitrate int `yaml:"bitrate"`
}

// MicrophoneEntry represents a single microphone declaration in devices.yaml.
type MicrophoneEntry struct {
	// UID is the udev ID_SERIAL of the USB device.  Same value as the
	// camera UID when the microphone is built into a USB camera.
	UID string `yaml:"uid"`

	// Name overrides the track name published to LiveKit.
	Name string `yaml:"name"`

	// SampleRate sets the capture sample rate in Hz (e.g. 48000).
	// When zero the device's maximum supported rate is used.
	SampleRate int `yaml:"sample_rate"`

	// Channels sets the number of capture channels (1 = mono, 2 = stereo).
	// When zero the device's maximum supported channel count is used.
	Channels int `yaml:"channels"`

	// Disabled prevents this microphone from being started.
	// When absent or false the microphone is enabled by default.
	Disabled bool `yaml:"disabled"`

	// Stream holds streaming-specific overrides (bitrate).
	Stream MicrophoneStreamCfg `yaml:"stream"`
}

// DevicesConfig is the top-level structure of devices.yaml.
type DevicesConfig struct {
	Cameras     []CameraEntry     `yaml:"cameras"`
	Microphones []MicrophoneEntry `yaml:"microphones"`
}

// LoadDevicesConfig reads and parses devices.yaml from the working directory.
// Returns an empty config (and no error) when the file does not exist, so
// callers can treat a missing file as "accept all devices".
func LoadDevicesConfig() (DevicesConfig, error) {
	var cfg DevicesConfig
	data, err := os.ReadFile("devices.yaml")
	if os.IsNotExist(err) {
		return cfg, nil
	}
	if err != nil {
		return cfg, err
	}
	return cfg, yaml.Unmarshal(data, &cfg)
}
