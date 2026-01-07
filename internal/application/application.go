package application

import (
	"context"
	"fmt"
	"log"
	"os"

	"github.com/geCarlo/gobelisk/internal/config"
	"github.com/geCarlo/gobelisk/internal/recorder"
)

// Run is the main application entrypoint for the gobelisk service.
// It starts the recorder and runs until ctx is cancelled (systemd stop / SIGTERM).
func Run(ctx context.Context, cfg *config.Config, logger *log.Logger) error {
	if cfg == nil {
		return fmt.Errorf("nil config")
	}
	if logger == nil {
		// Don't panic if caller forgot; default to stdlib logger.
		logger = log.New(os.Stdout, "", log.LstdFlags)
	}

	// Ensure the output directory exists.
	if err := os.MkdirAll(cfg.Recorder.OutputDir, 0o755); err != nil {
		return fmt.Errorf("create output dir %q: %w", cfg.Recorder.OutputDir, err)
	}

	rec := recorder.NewFFmpegRecorder(cfg, logger)

	logger.Printf(
		"recorder starting: device=%s vcodec=%s output_dir=%s segment_duration=%s",
		cfg.Device,
		cfg.VideoCodec,
		cfg.Recorder.OutputDir,
		cfg.Recorder.SegmentDuration,
	)

	// The recorder's Run should block until ctx is cancelled or ffmpeg exits.
	if err := rec.Run(ctx); err != nil {
		return fmt.Errorf("recorder error: %w", err)
	}

	logger.Printf("recorder stopped")
	return nil
}
