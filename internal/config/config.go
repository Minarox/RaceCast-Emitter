package config

import (
	"fmt"
	"os"

	"gopkg.in/yaml.v3"
)

// StreamConfig holds streaming parameters. Width, Height, Framerate, Channels fall back to
// capture values if unset. A missing or zero Bitrate disables streaming.
type StreamConfig struct {
	Width     int `yaml:"width"`
	Height    int `yaml:"height"`
	Framerate int `yaml:"framerate"`
	Bitrate   int `yaml:"bitrate"`
	Channels  int `yaml:"channels"` // audio only
}

type Camera struct {
	UID            string        `yaml:"uid"`
	Name           string        `yaml:"name"`
	Disabled       bool          `yaml:"disabled"`
	Width          int           `yaml:"width"`
	Height         int           `yaml:"height"`
	Framerate      int           `yaml:"framerate"`
	VerticalFlip   bool          `yaml:"vertical_flip"`
	HorizontalFlip bool          `yaml:"horizontal_flip"`
	Format         string        `yaml:"format"` // optional: "YUY2"/"YUYV" if MJPEG is unsupported
	Stream         *StreamConfig `yaml:"stream"`
	// Main marks the camera the shared streaming bandwidth budget always
	// protects first (see internal/pipeline/bandwidth.go's BandwidthCoordinator):
	// it's never paused for lack of bandwidth, and gets first claim on the
	// shared budget before any other camera gets a share. At most one camera
	// may set this — see validate(). Meaningless for a single-camera setup.
	Main bool `yaml:"main"`
}

func (c Camera) HasStream() bool {
	return c.Stream != nil && c.Stream.Bitrate > 0
}

func (c Camera) StreamWidth() int {
	if c.Stream != nil && c.Stream.Width > 0 {
		return c.Stream.Width
	}
	return c.Width
}

func (c Camera) StreamHeight() int {
	if c.Stream != nil && c.Stream.Height > 0 {
		return c.Stream.Height
	}
	return c.Height
}

func (c Camera) StreamFramerate() int {
	if c.Stream != nil && c.Stream.Framerate > 0 {
		return c.Stream.Framerate
	}
	return c.Framerate
}

func (c Camera) StreamBitrate() int {
	if c.Stream != nil {
		return c.Stream.Bitrate
	}
	return 0
}

type Microphone struct {
	UID        string        `yaml:"uid"`
	Name       string        `yaml:"name"`
	Disabled   bool          `yaml:"disabled"`
	SampleRate int           `yaml:"sample_rate"`
	Channels   int           `yaml:"channels"`
	Stream     *StreamConfig `yaml:"stream"`
}

func (m Microphone) HasStream() bool {
	return m.Stream != nil && m.Stream.Bitrate > 0
}

func (m Microphone) StreamBitrate() int {
	if m.Stream != nil {
		return m.Stream.Bitrate
	}
	return 0
}

// StreamChannels returns the channel count to encode for streaming, falling back to the
// capture Channels if unset. Lets a stereo capture be downmixed to mono for the stream
// (e.g. to save bitrate) while the local recording keeps full stereo.
func (m Microphone) StreamChannels() int {
	if m.Stream != nil && m.Stream.Channels > 0 {
		return m.Stream.Channels
	}
	return m.Channels
}

type Config struct {
	Cameras     []Camera     `yaml:"cameras"`
	Microphones []Microphone `yaml:"microphones"`
}

// Load reads and parses the YAML configuration file.
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading config file: %w", err)
	}
	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parsing config file: %w", err)
	}
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	return &cfg, nil
}

// validate checks invariants yaml.Unmarshal cannot enforce on its own. uid
// and name must be non-empty and unique within their own list (cameras and
// microphones are checked separately — the same uid/name reused across the
// two lists is fine, e.g. a webcam's video and audio interfaces sharing one
// udev serial). This matters beyond a friendly error message: uid is used as
// a map key for pipeline.Slot lookup, and name feeds directly into the
// inter-element channel name, the SRT streamid, and the IDR-request routing
// table (see build.go's interChannel/srtCallerURI and main.go's
// cameraByName) — a duplicate would silently interleave two physical
// devices' frames into one recording/stream instead of failing loudly here.
func (c *Config) validate() error {
	seenUID := make(map[string]bool, len(c.Cameras))
	seenName := make(map[string]bool, len(c.Cameras))
	mainSeen := ""
	for i, cam := range c.Cameras {
		if cam.UID == "" {
			return fmt.Errorf("camera %d: uid is required", i+1)
		}
		if cam.Name == "" {
			return fmt.Errorf("camera %d (uid %q): name is required", i+1, cam.UID)
		}
		if seenUID[cam.UID] {
			return fmt.Errorf("camera %q: duplicate uid %q", cam.Name, cam.UID)
		}
		seenUID[cam.UID] = true
		if seenName[cam.Name] {
			return fmt.Errorf("camera %q: duplicate name (uid %q)", cam.Name, cam.UID)
		}
		seenName[cam.Name] = true

		if cam.Main {
			if mainSeen != "" {
				return fmt.Errorf("camera %q: only one camera may be marked main (already set on %q)", cam.Name, mainSeen)
			}
			mainSeen = cam.Name
		}

		// Capture parameters feed straight into v4l2src/nvvidconv caps
		// filters in build.go — a missing/zero value here would otherwise
		// only surface as an opaque gst_parse_launch or caps-negotiation
		// failure once the pipeline actually starts. Skipped for disabled
		// cameras: a disabled entry never builds a pipeline, so it's a valid
		// placeholder (e.g. "not wired up yet") with these fields left unset.
		if !cam.Disabled {
			if cam.Width <= 0 || cam.Height <= 0 {
				return fmt.Errorf("camera %q: width and height must be positive", cam.Name)
			}
			if cam.Framerate <= 0 {
				return fmt.Errorf("camera %q: framerate must be positive", cam.Name)
			}
		}
	}

	seenUID = make(map[string]bool, len(c.Microphones))
	seenName = make(map[string]bool, len(c.Microphones))
	for i, mic := range c.Microphones {
		if mic.UID == "" {
			return fmt.Errorf("microphone %d: uid is required", i+1)
		}
		if mic.Name == "" {
			return fmt.Errorf("microphone %d (uid %q): name is required", i+1, mic.UID)
		}
		if seenUID[mic.UID] {
			return fmt.Errorf("microphone %q: duplicate uid %q", mic.Name, mic.UID)
		}
		seenUID[mic.UID] = true
		if seenName[mic.Name] {
			return fmt.Errorf("microphone %q: duplicate name (uid %q)", mic.Name, mic.UID)
		}
		seenName[mic.Name] = true

		if !mic.Disabled {
			if mic.SampleRate <= 0 {
				return fmt.Errorf("microphone %q: sample_rate must be positive", mic.Name)
			}
			if mic.Channels <= 0 {
				return fmt.Errorf("microphone %q: channels must be positive", mic.Name)
			}
		}
	}

	return nil
}
