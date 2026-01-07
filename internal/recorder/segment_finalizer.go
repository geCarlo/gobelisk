package recorder

import (
	"context"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

type SegmentFinalizer struct {
	OutputDir       string
	FilenamePattern string // e.g. "segment-%Y-%m-%d_%H-%M-%S.mkv"
	SegmentDuration time.Duration
	PollInterval    time.Duration
	StableFor       time.Duration
	OfflineSuffix   string
	HB              *hbState
	Logger          func(string, ...any)
	Enabled         bool

	ext string // derived
}

func parseSegStart(name string) (time.Time, bool) {
	ext := filepath.Ext(name)
	base := strings.TrimSuffix(name, ext)
	base = strings.TrimSuffix(base, "-OFFLINE")
	base = strings.TrimPrefix(base, "segment-")

	t, err := time.ParseInLocation("2006-01-02_15-04-05", base, time.Local)
	if err != nil {
		return time.Time{}, false
	}
	return t, true
}

// Run polls the output directory and renames completed segments with -OFFLINE
// when heartbeat indicates the client host is down.
//
// NOTE: This "simple" version decides offline based on HB.up at finalize time.
func (f *SegmentFinalizer) Run(ctx context.Context) {
	if f.HB == nil {
		return
	}
	if f.Logger == nil {
		f.Logger = func(string, ...any) {}
	}
	if f.PollInterval <= 0 {
		f.PollInterval = 1 * time.Second
	}
	if f.StableFor <= 0 {
		f.StableFor = 2 * time.Second
	}
	if f.OfflineSuffix == "" {
		f.OfflineSuffix = "-OFFLINE"
	}
	if f.FilenamePattern == "" {
		f.Logger("segment finalizer: empty filename pattern; not running")
		return
	}
	f.ext = strings.ToLower(filepath.Ext(f.FilenamePattern))
	if f.ext == "" {
		f.Logger("segment finalizer: filename pattern has no extension: %q", f.FilenamePattern)
		return
	}

	t := time.NewTicker(f.PollInterval)
	defer t.Stop()

	// Track last-seen stats so we can detect stability.
	type snap struct {
		size       int64
		modTime    time.Time
		lastChange time.Time
	}
	seen := map[string]*snap{}

	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			entries, err := os.ReadDir(f.OutputDir)
			if err != nil {
				f.Logger("segment finalizer: readdir %s: %v", f.OutputDir, err)
				continue
			}

			// Sort to make behavior stable/predictable (optional)
			sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })

			now := time.Now()

			for _, e := range entries {
				if e.IsDir() {
					continue
				}
				name := e.Name()

				// Only handle the chosen extension
				if !strings.HasSuffix(strings.ToLower(name), strings.ToLower(f.ext)) {
					continue
				}
				// Skip already-tagged files
				if strings.Contains(name, f.OfflineSuffix) {
					continue
				}

				full := filepath.Join(f.OutputDir, name)

				info, err := e.Info()
				if err != nil {
					continue
				}

				s, ok := seen[full]
				if !ok {
					seen[full] = &snap{
						size:       info.Size(),
						modTime:    info.ModTime(),
						lastChange: now,
					}
					continue
				}

				changed := info.Size() != s.size || !info.ModTime().Equal(s.modTime)
				if changed {
					s.size = info.Size()
					s.modTime = info.ModTime()
					s.lastChange = now
					continue
				}

				// Stable long enough => treat as "closed"
				if now.Sub(s.lastChange) < f.StableFor {
					continue
				}

				// Decide whether to tag offline
				// isUp := f.HB.up.Load()
				// if !isUp {
				// 	newName := addSuffixBeforeExt(name, f.OfflineSuffix)
				// 	dst := filepath.Join(f.OutputDir, newName)

				// 	// Try rename
				// 	if err := os.Rename(full, dst); err != nil {
				// 		f.Logger("segment finalizer: rename %s -> %s failed: %v", full, dst, err)
				// 	} else {
				// 		f.Logger("segment finalizer: tagged offline: %s", newName)
				// 	}
				// }
				// Decide whether to tag offline (based on overlap with DOWN intervals)
				segStart, ok := parseSegStart(name)
				if !ok {
					// If we can't parse the timestamp, safest behavior is to do nothing.
					// (Or you could fall back to modtime-based logic.)
					f.Logger("segment finalizer: could not parse segment time from %q; skipping offline tagging", name)
				} else {
					segEnd := segStart.Add(f.SegmentDuration)

					if f.HB.overlapsOffline(segStart, segEnd) {
						newName := addSuffixBeforeExt(name, f.OfflineSuffix)
						dst := filepath.Join(f.OutputDir, newName)

						if err := os.Rename(full, dst); err != nil {
							f.Logger("segment finalizer: rename %s -> %s failed: %v", full, dst, err)
						} else {
							f.Logger("segment finalizer: tagged offline: %s", newName)
						}
					}
				}

				// Done tracking this file
				delete(seen, full)
			}

			// Clean up entries that disappeared
			for path := range seen {
				if _, err := os.Stat(path); err != nil {
					delete(seen, path)
				}
			}
		}
	}
}

func addSuffixBeforeExt(name, suffix string) string {
	ext := filepath.Ext(name)
	base := name[:len(name)-len(ext)]
	return base + suffix + ext
}
