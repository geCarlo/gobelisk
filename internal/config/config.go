package config

import (
	"fmt"
	"os"
	"time"

	"gopkg.in/yaml.v3"
)

type RecorderConfig struct {
	FrameRate       string        `yaml:"frame_rate"`
	VideoSize       string        `yaml:"video_size"`
	OutputDir       string        `yaml:"output_dir"`
	FilenamePattern string        `yaml:"filename_pattern"`
	SegmentDuration time.Duration `yaml:"segment_seconds"`
	Retention       time.Duration `yaml:"retention"`
	PurgeInterval   time.Duration `yaml:"purge_interval"`
}

type StreamerConfig struct {
	Enabled   bool   `yaml:"enabled"`
	FrameRate string `yaml:"frame_rate"`
	VideoSize string `yaml:"video_size"`
	BackupLoc string `yaml:"backup_loc"`
	ClientURL string `yaml:"client_url"`
}

type Config struct {
	Device       string   `yaml:"device"`
	InputFormat  string   `yaml:"input_format"`
	TimestampPos string   `yaml:"timestamp_pos"`
	VideoCodec   string   `yaml:"video_codec"`
	FFmpegPath   string   `yaml:"ffmpeg_path"`
	ExtraArgs    []string `yaml:"extra_args"`

	Recorder RecorderConfig `yaml:"recorder"`
	Streamer StreamerConfig `yaml:"streamer"`
}

// Load reads a YAML config file from disk and returns a validated Config.
func Load(path string) (*Config, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config %q: %w", path, err)
	}

	var cfg Config
	if err := yaml.Unmarshal(b, &cfg); err != nil {
		return nil, fmt.Errorf("parse yaml: %w", err)
	}

	applyDefaults(&cfg)
	if err := validate(&cfg); err != nil {
		return nil, err
	}

	return &cfg, nil
}

func applyDefaults(cfg *Config) {
	if cfg.Recorder.SegmentDuration == 0 {
		cfg.Recorder.SegmentDuration = 120 * time.Second
	}
	if cfg.VideoCodec == "" {
		cfg.VideoCodec = "copy"
	}
	// AudioCodec empty is intentional (many cameras have no audio)
}

func validate(cfg *Config) error {
	r := cfg

	if r.Device == "" {
		return fmt.Errorf("recorder.device is required")
	}
	if r.Recorder.OutputDir == "" {
		return fmt.Errorf("recorder.output_dir is required")
	}
	if r.Recorder.SegmentDuration <= 0 {
		return fmt.Errorf("recorder.segment_duration must be > 0")
	}

	return nil
}
