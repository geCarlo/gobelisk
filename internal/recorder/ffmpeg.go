package recorder

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/geCarlo/gobelisk/internal/config"
)

type FFmpegRecorder struct {
	cfg    *config.Config
	logger *log.Logger
	hb     *hbState
}

func NewFFmpegRecorder(cfg *config.Config, logger *log.Logger) *FFmpegRecorder {
	if logger == nil {
		logger = log.New(os.Stdout, "", log.LstdFlags)
	}
	return &FFmpegRecorder{cfg: cfg, logger: logger, hb: &hbState{}}
}

func hostFromClientURL(clientURL string) (string, error) {
	u, err := url.Parse(strings.TrimSpace(clientURL))
	if err != nil {
		return "", err
	}
	host := u.Hostname()
	if host == "" {
		return "", fmt.Errorf("client_url missing host: %q", clientURL)
	}
	return host, nil
}

func dialReachable(host string, port int, timeout time.Duration) bool {
	addr := net.JoinHostPort(host, strconv.Itoa(port))
	c, err := net.DialTimeout("tcp", addr, timeout)
	if err == nil {
		_ = c.Close()
		return true // connected => definitely reachable
	}

	// Treat "connection refused" as reachable (RST received)
	var opErr *net.OpError
	if errors.As(err, &opErr) {
		// Most refused connections end up as syscall.ECONNREFUSED
		var sysErr *os.SyscallError
		if errors.As(opErr.Err, &sysErr) && sysErr.Err == syscall.ECONNREFUSED {
			return true
		}
		// Sometimes the syscall error is directly in opErr.Err
		if errors.Is(opErr.Err, syscall.ECONNREFUSED) {
			return true
		}
	}

	// Other errors: timeout, no route, host unreachable, etc => DOWN
	return false
}

type hbInterval struct {
	start time.Time
	end   time.Time // zero = still down
}

type hbState struct {
	up atomic.Bool

	mu        sync.Mutex
	intervals []hbInterval
}

func (h *hbState) markDown(ts time.Time) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.intervals) > 0 && h.intervals[len(h.intervals)-1].end.IsZero() {
		return // already down
	}
	h.intervals = append(h.intervals, hbInterval{start: ts})
}

func (h *hbState) markUp(ts time.Time) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.intervals) == 0 {
		return
	}
	last := &h.intervals[len(h.intervals)-1]
	if last.end.IsZero() {
		last.end = ts
	}
}

func (h *hbState) overlapsOffline(start, end time.Time) bool {
	h.mu.Lock()
	defer h.mu.Unlock()

	now := time.Now()
	for _, in := range h.intervals {
		iEnd := in.end
		if iEnd.IsZero() {
			iEnd = now
		}
		if start.Before(iEnd) && end.After(in.start) {
			return true
		}
	}
	return false
}

func (r *FFmpegRecorder) startHeartbeat(ctx context.Context, clientURL string, st *hbState) {
	host, err := hostFromClientURL(clientURL)
	if err != nil {
		r.logger.Printf("heartbeat: disabled (bad client_url): %v", err)
		return
	}

	port := 22
	interval := 2 * time.Second
	timeout := 800 * time.Millisecond
	downAfter := 3
	upAfter := 2

	// Initialize from a real probe (better than assuming up)
	initialUp := dialReachable(host, port, timeout)
	st.up.Store(initialUp)

	r.logger.Printf("heartbeat: checking reachability host=%s port=%d interval=%s timeout=%s (initial=%v)",
		host, port, interval, timeout, initialUp)

	go func() {
		t := time.NewTicker(interval)
		defer t.Stop()

		fails := 0
		oks := 0
		last := st.up.Load()

		// helper to run one probe
		check := func() {
			ok := dialReachable(host, port, timeout)
			if ok {
				fails = 0
				oks++
			} else {
				oks = 0
				fails++
			}

			cur := st.up.Load()
			if cur && fails >= downAfter {
				st.up.Store(false)
			} else if !cur && oks >= upAfter {
				st.up.Store(true)
			}

			now := st.up.Load()
			if now != last {
				last = now
				if now {
					st.markUp(time.Now())
					r.logger.Printf("heartbeat: UP (reachable) host=%s port=%d", host, port)
				} else {
					st.markDown(time.Now())
					r.logger.Printf("heartbeat: DOWN (unreachable) host=%s port=%d", host, port)
				}
			}
		}

		// Do one check quickly after start so state converges immediately
		check()

		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				check()
			}
		}
	}()
}

// Run starts ffmpeg and blocks until ctx is cancelled or ffmpeg exits.
// IMPORTANT: We intentionally DO NOT use exec.CommandContext because that can
// SIGKILL ffmpeg on ctx cancellation, which corrupts MP4 (moov atom missing).
func (r *FFmpegRecorder) Run(ctx context.Context) error {
	if r.cfg.FFmpegPath == "" {
		r.cfg.FFmpegPath = "ffmpeg"
	}

	// ---- Defaults ----
	if strings.TrimSpace(r.cfg.InputFormat) == "" {
		// Your YAML often used yuyv422 or mjpeg; pick your preferred default
		r.cfg.InputFormat = "yuyv422"
	}
	if strings.TrimSpace(r.cfg.VideoCodec) == "" {
		r.cfg.VideoCodec = "libx264"
	}
	if strings.TrimSpace(r.cfg.TimestampPos) == "" {
		r.cfg.TimestampPos = "tr"
	}

	// Recorder defaults
	if strings.TrimSpace(r.cfg.Recorder.FrameRate) == "" {
		r.cfg.Recorder.FrameRate = "15"
	}
	if strings.TrimSpace(r.cfg.Recorder.VideoSize) == "" {
		r.cfg.Recorder.VideoSize = "640x480"
	}
	if r.cfg.Recorder.SegmentDuration <= 0 {
		r.cfg.Recorder.SegmentDuration = 120 * time.Second
	}

	// ---- Validation ----
	if strings.TrimSpace(r.cfg.Device) == "" {
		return fmt.Errorf("general.device (cfg.Device) is required")
	}
	if strings.TrimSpace(r.cfg.Recorder.OutputDir) == "" {
		return fmt.Errorf("recorder.output_dir (cfg.OutputDir) is required")
	}
	if strings.TrimSpace(r.cfg.Recorder.FilenamePattern) == "" {
		r.cfg.Recorder.FilenamePattern = "segment-%Y-%m-%d_%H-%M-%S.mkv"
	}
	if err := os.MkdirAll(r.cfg.Recorder.OutputDir, 0o755); err != nil {
		return fmt.Errorf("mkdir %q: %w", r.cfg.Recorder.OutputDir, err)
	}

	if err := mustPositiveIntString("recorder.frame_rate", r.cfg.Recorder.FrameRate); err != nil {
		return err
	}
	if r.cfg.Streamer.Enabled {
		if err := mustPositiveIntString("streamer.frame_rate", r.cfg.Streamer.FrameRate); err != nil {
			return err
		}
	}

	// Streamer defaults (only matter if enabled)
	if r.cfg.Streamer.Enabled {
		if strings.TrimSpace(r.cfg.Streamer.FrameRate) == "" {
			r.cfg.Streamer.FrameRate = r.cfg.Recorder.FrameRate
		}
		if strings.TrimSpace(r.cfg.Streamer.VideoSize) == "" {
			r.cfg.Streamer.VideoSize = r.cfg.Recorder.VideoSize
		}
		if strings.TrimSpace(r.cfg.Streamer.ClientURL) == "" {
			// local UDP default
			r.cfg.Streamer.ClientURL = "udp://127.0.0.1:5000?pkt_size=1316"
			r.logger.Printf("No URL found for client, disabling hearbeat")
		} else {
			r.startHeartbeat(ctx, r.cfg.Streamer.ClientURL, r.hb)
		}

		// start finalizer
		go (&SegmentFinalizer{
			Enabled:         true,
			OutputDir:       r.cfg.Recorder.OutputDir,
			FilenamePattern: r.cfg.Recorder.FilenamePattern,
			SegmentDuration: r.cfg.Recorder.SegmentDuration,
			PollInterval:    1 * time.Second,
			StableFor:       2 * time.Second,
			OfflineSuffix:   "-OFFLINE",
			HB:              r.hb,
			Logger:          r.logger.Printf,
		}).Run(ctx)
	}

	go (&Purger{
		Enabled:         true, // or cfg.Recorder.PurgeEnabled
		OutputDir:       r.cfg.Recorder.OutputDir,
		FileNamePattern: r.cfg.Recorder.FilenamePattern,
		Retention:       r.cfg.Recorder.Retention,
		Interval:        r.cfg.Recorder.PurgeInterval,
		MinAge:          30 * time.Second,
		Logger:          r.logger.Printf,
	}).Run(ctx)

	// Output pattern for segment muxer.
	outputPattern := filepath.Join(r.cfg.Recorder.OutputDir, r.cfg.Recorder.FilenamePattern)

	args := r.buildArgs(outputPattern)

	cmd := exec.Command(r.cfg.FFmpegPath, args...)
	cmd.Stdout = io.Discard
	cmd.Stderr = os.Stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	r.logger.Printf("starting ffmpeg: %s %v", r.cfg.FFmpegPath, args)

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("ffmpeg start: %w", err)
	}

	waitCh := make(chan error, 1)
	go func() { waitCh <- cmd.Wait() }()

	// We are always writing MP4 segments in this design.
	isMP4 := looksLikeMP4(outputPattern)

	select {
	case <-ctx.Done():
		r.logger.Printf("stopping ffmpeg (ctx cancelled)...")

		// MP4 needs more time to finalize the moov atom / close the file cleanly.
		totalGrace := 8 * time.Second
		if isMP4 {
			totalGrace = 30 * time.Second
		}

		_ = terminateProcessGroup(cmd.Process.Pid, totalGrace, r.logger)
		<-waitCh
		return nil

	case err := <-waitCh:
		if err != nil {
			return fmt.Errorf("ffmpeg exited with error: %w", err)
		}
		return nil
	}
}

func mustPositiveIntString(name, s string) error {
	n, err := strconv.Atoi(strings.TrimSpace(s))
	if err != nil || n <= 0 {
		return fmt.Errorf("%s must be a positive integer string (got %q)", name, s)
	}
	return nil
}

func looksLikeMP4(path string) bool {
	l := strings.ToLower(strings.TrimSpace(path))
	return strings.HasSuffix(l, ".mp4") || strings.HasSuffix(l, ".m4v") || strings.HasSuffix(l, ".mov")
}

func (r *FFmpegRecorder) buildArgs(outputPattern string) []string {
	vc := strings.ToLower(strings.TrimSpace(r.cfg.VideoCodec))
	isCopy := (vc == "copy")

	// If streamer settings match recorder settings, we can encode once and tee the output.
	canTee := r.cfg.Streamer.Enabled &&
		strings.TrimSpace(r.cfg.Streamer.FrameRate) == strings.TrimSpace(r.cfg.Recorder.FrameRate) &&
		strings.TrimSpace(r.cfg.Streamer.VideoSize) == strings.TrimSpace(r.cfg.Recorder.VideoSize) &&
		!isCopy // tee is best with a real encoder pipeline here

	// ---- Input (open device ONCE) ----
	args := []string{
		"-hide_banner",
		"-nostdin",
		"-loglevel", "warning",

		"-f", "v4l2",
		"-input_format", r.cfg.InputFormat,
		"-video_size", r.cfg.Recorder.VideoSize,
		"-framerate", r.cfg.Recorder.FrameRate,
		"-i", r.cfg.Device,
	}

	// ---- Base filter (timestamp, pixel format) ----
	baseVF := ""
	if !isCopy {
		inputFmt := strings.ToLower(strings.TrimSpace(r.cfg.InputFormat))

		baseVF = "format=yuv420p"
		if inputFmt == "mjpeg" || inputFmt == "mjpg" {
			baseVF = "scale=in_range=jpeg:out_range=tv,format=yuv420p"
		}

		pos := strings.ToLower(strings.TrimSpace(r.cfg.TimestampPos))
		if pos == "" {
			pos = "tr"
		}
		xExpr, yExpr := "10", "10"
		switch pos {
		case "tr":
			xExpr, yExpr = "w-tw-10", "10"
		case "bl":
			xExpr, yExpr = "10", "h-th-10"
		case "br":
			xExpr, yExpr = "w-tw-10", "h-th-10"
		case "tl":
			// defaults
		}

		baseVF += ",drawtext=fontfile=/usr/share/fonts/truetype/dejavu/DejaVuSans.ttf" +
			":expansion=strftime" +
			":text=%Y-%m-%d\\ %H\\\\:%M\\\\:%S" +
			":x=" + xExpr + ":y=" + yExpr +
			":fontsize=24:box=1:boxborderw=6"
	}

	// Always apply -vf when encoding (filters force re-encode anyway).
	if baseVF != "" {
		args = append(args, "-vf", baseVF)
	}

	// ============================================
	// ALWAYS RECORD
	// OPTIONALLY STREAM
	// ============================================

	if !r.cfg.Streamer.Enabled {
		// Record-only (your existing record path, with recorder.* fields)
		return r.appendRecordOutput(args, outputPattern)
	}

	// Stream enabled:
	if canTee {
		// Encode once -> tee to segment files + stream URL.
		return r.appendTeeRecordAndStream(args, outputPattern)
	}

	// Stream enabled, but different fps/size requested:
	// Do a single capture, then split into two branches and encode twice.
	return r.appendSplitRecordAndStream(args, outputPattern)
}

func (r *FFmpegRecorder) appendRecordOutput(args []string, outputPattern string) []string {
	vc := strings.ToLower(strings.TrimSpace(r.cfg.VideoCodec))
	args = append(args, "-c:v", r.cfg.VideoCodec)

	// Safe defaults for libx264 (recording)
	if vc == "libx264" {
		if !containsArg(r.cfg.ExtraArgs, "-preset") {
			args = append(args, "-preset", "superfast")
		}
		if !containsArg(r.cfg.ExtraArgs, "-crf") && !containsArg(r.cfg.ExtraArgs, "-b:v") {
			args = append(args, "-crf", "23")
		}
		if looksLikeMP4(outputPattern) {
			if !containsArg(r.cfg.ExtraArgs, "-pix_fmt") {
				args = append(args, "-pix_fmt", "yuv420p")
			}
			if !containsArg(r.cfg.ExtraArgs, "-movflags") {
				args = append(args, "-movflags", "+faststart")
			}
		}
	}

	// Segment muxer options
	args = append(args,
		"-f", "segment",
		"-segment_time", fmt.Sprintf("%.0f", r.cfg.Recorder.SegmentDuration.Seconds()),
		"-reset_timestamps", "1",
		"-strftime", "1",
	)

	if len(r.cfg.ExtraArgs) > 0 {
		args = append(args, r.cfg.ExtraArgs...)
	}

	args = append(args, outputPattern)
	return args
}

func (r *FFmpegRecorder) appendTeeRecordAndStream(args []string, outputPattern string) []string {
	clientURL := strings.TrimSpace(r.cfg.Streamer.ClientURL)
	if clientURL == "" {
		clientURL = "udp://127.0.0.1:5000?pkt_size=1316"
	}

	// Encode settings tuned to be decent for recording and acceptable for streaming.
	// You can tweak preset/crf based on Pi model.
	args = append(args,
		"-c:v", "libx264",
		"-preset", "superfast",
		"-crf", "23",
		"-pix_fmt", "yuv420p",
	)

	// GOP for streaming friendliness (optional but helpful)
	// 30 frames ~ 2s at 15fps
	args = append(args, "-g", "30", "-bf", "0")

	// Build tee destinations:
	// - segment muxer for files
	// - mpegts muxer for UDP stream
	seg := fmt.Sprintf("[f=segment:segment_time=%.0f:reset_timestamps=1:strftime=1]%s",
		r.cfg.Recorder.SegmentDuration.Seconds(),
		escapeTeePath(outputPattern),
	)
	str := fmt.Sprintf("[f=mpegts]%s", escapeTeePath(clientURL))

	teeOut := seg + "|" + str

	args = append(args,
		"-f", "tee",
		teeOut,
	)

	return args
}

func (r *FFmpegRecorder) appendSplitRecordAndStream(args []string, outputPattern string) []string {
	clientURL := strings.TrimSpace(r.cfg.Streamer.ClientURL)
	if clientURL == "" {
		clientURL = "udp://127.0.0.1:5000?pkt_size=1316"
	}

	// We already applied baseVF via -vf above. For split+dual encode, we need filter_complex instead,
	// so we should remove that and rebuild with -filter_complex.
	//
	// Easiest way: rebuild args up to "-i" and then use filter_complex for both paths.
	//
	// Since args already has -vf, strip it if present.
	args = stripArgPair(args, "-vf")

	// Build a filter graph:
	//  - apply base filters once
	//  - split
	//  - branch A: recorder at recorder fps/size (usually unchanged)
	//  - branch B: streamer fps/size (may differ)
	base := buildBaseFilterGraph(r.cfg.InputFormat, r.cfg.TimestampPos)

	recSize := strings.TrimSpace(r.cfg.Recorder.VideoSize)
	recFPS := strings.TrimSpace(r.cfg.Recorder.FrameRate)

	strSize := strings.TrimSpace(r.cfg.Streamer.VideoSize)
	strFPS := strings.TrimSpace(r.cfg.Streamer.FrameRate)

	// Note: fps filter expects fps as number; we pass as string.
	filter := fmt.Sprintf(
		"[0:v]%s,split=2[vrec][vstr];"+
			"[vrec]scale=%s,fps=%s[vrecout];"+
			"[vstr]scale=%s,fps=%s[vstrout]",
		base,
		recSizeToScale(recSize), recFPS,
		recSizeToScale(strSize), strFPS,
	)

	args = append(args, "-filter_complex", filter)

	// Map + encode recorder branch
	args = append(args,
		"-map", "[vrecout]",
		"-c:v:0", "libx264",
		"-preset:v:0", "superfast",
		"-crf:v:0", "23",
		"-pix_fmt:v:0", "yuv420p",
	)

	// Segment muxer for output 0
	args = append(args,
		"-f", "segment",
		"-segment_time", fmt.Sprintf("%.0f", r.cfg.Recorder.SegmentDuration.Seconds()),
		"-reset_timestamps", "1",
		"-strftime", "1",
		outputPattern,
	)

	// Map + encode streamer branch (low latency)
	args = append(args,
		"-map", "[vstrout]",
		"-c:v", "libx264",
		"-preset", "ultrafast",
		"-tune", "zerolatency",
		"-bf", "0",
		"-g", "30",
		"-pix_fmt", "yuv420p",
		"-f", "mpegts",
		clientURL,
	)

	return args
}

// buildBaseFilterGraph returns the common filter graph (without leading/trailing commas).
func buildBaseFilterGraph(inputFormat, timestampPos string) string {
	inputFmt := strings.ToLower(strings.TrimSpace(inputFormat))

	parts := []string{}

	// For MJPEG, optional range conversion + yuv420p
	if inputFmt == "mjpeg" || inputFmt == "mjpg" {
		parts = append(parts, "scale=in_range=jpeg:out_range=tv")
	}
	parts = append(parts, "format=yuv420p")

	// Timestamp overlay
	pos := strings.ToLower(strings.TrimSpace(timestampPos))
	if pos == "" {
		pos = "tr"
	}
	xExpr, yExpr := "10", "10"
	switch pos {
	case "tr":
		xExpr, yExpr = "w-tw-10", "10"
	case "bl":
		xExpr, yExpr = "10", "h-th-10"
	case "br":
		xExpr, yExpr = "w-tw-10", "h-th-10"
	case "tl":
		// defaults
	}

	draw := "drawtext=fontfile=/usr/share/fonts/truetype/dejavu/DejaVuSans.ttf" +
		":expansion=strftime" +
		":text=%Y-%m-%d\\ %H\\\\:%M\\\\:%S" +
		":x=" + xExpr + ":y=" + yExpr +
		":fontsize=24:box=1:boxborderw=6"

	parts = append(parts, draw)

	return strings.Join(parts, ",")
}

// ffmpeg's scale filter wants "WIDTH:HEIGHT". We accept "640x480" and convert to "640:480".
// If format isn't recognized, just pass it through with ":" replaced as a best effort.
func recSizeToScale(sz string) string {
	sz = strings.TrimSpace(sz)
	if sz == "" {
		return "640:480"
	}
	// common "640x480" -> "640:480"
	return strings.ReplaceAll(sz, "x", ":")
}

// tee output string uses special escaping rules; simplest is to escape ':' in local paths.
// For URLs like udp://, ffmpeg tee also treats ':' specially; escaping is still helpful.
// This is a best-effort and works for typical cases.
func escapeTeePath(s string) string {
	// For tee muxer, ':' separates options; escaping with '\:' is common.
	// Also escape '\' itself first.
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `:`, `\:`)
	return s
}

func stripArgPair(args []string, key string) []string {
	out := make([]string, 0, len(args))
	for i := 0; i < len(args); i++ {
		if args[i] == key {
			// drop key and its value if present
			if i+1 < len(args) {
				i++
			}
			continue
		}
		out = append(out, args[i])
	}
	return out
}

func containsArg(args []string, needle string) bool {
	for i := 0; i < len(args); i++ {
		if args[i] == needle {
			return true
		}
	}
	return false
}

// terminateProcessGroup tries SIGINT then SIGTERM before SIGKILL.
// For MP4, this is critical so ffmpeg can flush/close the moov atom.
func terminateProcessGroup(pid int, totalGrace time.Duration, logger *log.Logger) error {
	pgid := -pid

	intGrace := totalGrace * 70 / 100
	termGrace := totalGrace - intGrace
	if intGrace < 2*time.Second {
		intGrace = 2 * time.Second
	}
	if termGrace < 2*time.Second {
		termGrace = 2 * time.Second
	}

	_ = syscall.Kill(pgid, syscall.SIGINT)
	if waitGroupExit(pgid, intGrace) {
		return nil
	}

	_ = syscall.Kill(pgid, syscall.SIGTERM)
	if waitGroupExit(pgid, termGrace) {
		return nil
	}

	logger.Printf("ffmpeg did not exit within %s (SIGINT+SIGTERM); sending SIGKILL", totalGrace)
	_ = syscall.Kill(pgid, syscall.SIGKILL)
	return nil
}

func waitGroupExit(pgid int, grace time.Duration) bool {
	deadline := time.Now().Add(grace)
	for time.Now().Before(deadline) {
		if err := syscall.Kill(pgid, 0); err != nil {
			return true
		}
		time.Sleep(100 * time.Millisecond)
	}
	return false
}
