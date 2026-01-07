package recorder

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"time"
)

type Purger struct {
	Enabled         bool
	OutputDir       string
	FileNamePattern string // to derive extension (".mkv")
	Retention       time.Duration
	Interval        time.Duration
	MinAge          time.Duration // safety: do not delete very recent files
	Logger          func(string, ...any)

	ext string
}

func (p *Purger) Run(ctx context.Context) {
	if !p.Enabled {
		return
	}
	if p.Logger == nil {
		p.Logger = func(string, ...any) {}
	}
	if p.Interval <= 0 {
		p.Interval = 10 * time.Minute
	}
	if p.MinAge <= 0 {
		p.MinAge = 30 * time.Second
	}
	if p.Retention <= 0 {
		p.Logger("purger: retention <= 0; not running")
		return
	}
	if p.OutputDir == "" {
		p.Logger("purger: output_dir empty; not running")
		return
	}

	p.ext = strings.ToLower(filepath.Ext(p.FileNamePattern))
	if p.ext == "" {
		p.Logger("purger: filename pattern has no extension: %q; not running", p.FileNamePattern)
		return
	}

	t := time.NewTicker(p.Interval)
	defer t.Stop()

	p.Logger("purger: enabled output_dir=%s retention=%s interval=%s", p.OutputDir, p.Retention, p.Interval)

	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			p.purgeOnce()
		}
	}
}

func (p *Purger) purgeOnce() {
	entries, err := os.ReadDir(p.OutputDir)
	if err != nil {
		p.Logger("purger: readdir %s: %v", p.OutputDir, err)
		return
	}

	now := time.Now()
	cutoff := now.Add(-p.Retention)

	var deleted int

	for _, e := range entries {
		if e.IsDir() {
			continue
		}

		name := e.Name()
		if !strings.HasSuffix(strings.ToLower(name), p.ext) {
			continue
		}

		full := filepath.Join(p.OutputDir, name)

		info, err := e.Info()
		if err != nil {
			continue
		}

		age := now.Sub(info.ModTime())

		// safety: never delete too recent (could be in-progress / just finalized)
		if age < p.MinAge {
			continue
		}

		// delete if older than retention
		if info.ModTime().Before(cutoff) {
			if err := os.Remove(full); err != nil {
				p.Logger("purger: remove %s: %v", full, err)
				continue
			}
			deleted++
		}
	}

	if deleted > 0 {
		p.Logger("purger: deleted %d files older than %s", deleted, p.Retention)
	}
}
