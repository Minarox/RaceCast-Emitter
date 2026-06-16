package config

import (
	"fmt"
	"os"

	"gopkg.in/yaml.v3"
)

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
	Format string `yaml:"format"`
}

type Microphone struct {
	UID        string `yaml:"uid"`
	Name       string `yaml:"name"`
	SampleRate int    `yaml:"sample_rate"`
	Channels   int    `yaml:"channels"`
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
