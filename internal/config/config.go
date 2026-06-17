package config

import (
	"fmt"
	"os"

	"gopkg.in/yaml.v3"
)

// StreamConfig contient les paramètres de diffusion d'un périphérique.
// Width, Height et Framerate sont optionnels : les valeurs de capture sont utilisées si absentes.
// Bitrate est obligatoire — si absent ou si la section stream est manquante, la diffusion est désactivée.
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
	// Format optionnel : "YUY2"/"YUYV" si la caméra ne supporte pas le MJPEG (MJPEG par défaut)
	Format string        `yaml:"format"`
	Stream *StreamConfig `yaml:"stream"`
}

// HasStream retourne true si la diffusion est activée (bitrate configuré).
func (c Camera) HasStream() bool { return c.Stream != nil && c.Stream.Bitrate > 0 }

// StreamWidth retourne la largeur de diffusion, en repliant sur la largeur de capture.
func (c Camera) StreamWidth() int {
	if c.Stream != nil && c.Stream.Width > 0 {
		return c.Stream.Width
	}
	return c.Width
}

// StreamHeight retourne la hauteur de diffusion, en repliant sur la hauteur de capture.
func (c Camera) StreamHeight() int {
	if c.Stream != nil && c.Stream.Height > 0 {
		return c.Stream.Height
	}
	return c.Height
}

// StreamFramerate retourne le framerate de diffusion, en repliant sur le framerate de capture.
func (c Camera) StreamFramerate() int {
	if c.Stream != nil && c.Stream.Framerate > 0 {
		return c.Stream.Framerate
	}
	return c.Framerate
}

// StreamBitrate retourne le bitrate de diffusion (0 si non configuré).
func (c Camera) StreamBitrate() int {
	if c.Stream != nil {
		return c.Stream.Bitrate
	}
	return 0
}

type Microphone struct {
	UID        string        `yaml:"uid"`
	Name       string        `yaml:"name"`
	SampleRate int           `yaml:"sample_rate"`
	Channels   int           `yaml:"channels"`
	Stream     *StreamConfig `yaml:"stream"`
}

// HasStream retourne true si la diffusion est activée (bitrate configuré).
func (m Microphone) HasStream() bool { return m.Stream != nil && m.Stream.Bitrate > 0 }

// StreamBitrate retourne le bitrate de diffusion Opus (0 si non configuré).
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

// Load charge et parse le fichier de configuration YAML.
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("lecture du fichier de configuration : %w", err)
	}
	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("analyse du fichier de configuration : %w", err)
	}
	return &cfg, nil
}
