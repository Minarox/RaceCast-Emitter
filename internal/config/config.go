package config

import (
	"fmt"
	"os"

	"gopkg.in/yaml.v3"
)

// StreamConfig holds streaming parameters. Width, Height, Framerate fall back to capture values if unset.
// A missing or zero Bitrate disables streaming.
type StreamConfig struct {
	Width     int `yaml:"width"`
	Height    int `yaml:"height"`
	Framerate int `yaml:"framerate"`
	Bitrate   int `yaml:"bitrate"`
}

type Camera struct {
	UID            string `yaml:"uid"`
	Name           string `yaml:"name"`
	Disabled       bool   `yaml:"disabled"`
	Width          int    `yaml:"width"`
	Height         int    `yaml:"height"`
	Framerate      int    `yaml:"framerate"`
	VerticalFlip   bool   `yaml:"vertical_flip"`
	HorizontalFlip bool   `yaml:"horizontal_flip"`
	Format         string        `yaml:"format"` // optional: "YUY2"/"YUYV" if MJPEG is unsupported
	Stream         *StreamConfig `yaml:"stream"`
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
	return &cfg, nil
}
