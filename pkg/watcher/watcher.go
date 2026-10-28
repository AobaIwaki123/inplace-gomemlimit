package watcher

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"runtime/debug"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	DefaultCgroupPath = "/sys/fs/cgroup/memory.max"
	DefaultRatio      = 0.90
	DefaultInterval   = 2 * time.Second
)

// Options configures the in-place GOMEMLIMIT watcher.
type Options struct {
	// CgroupPath path to cgroup v2 memory.max (defaults to /sys/fs/cgroup/memory.max)
	CgroupPath string
	// Ratio of cgroup limit to set as GOMEMLIMIT (e.g. 0.90 for 90%)
	Ratio float64
	// Interval checking interval for cgroup changes
	Interval time.Duration
	// Logger structured logger for observation
	Logger *slog.Logger
	// OnLimitChanged callback invoked when GOMEMLIMIT is updated
	OnLimitChanged func(oldLimit, newLimit int64, cgroupMax int64)
}

// Watcher monitors cgroup v2 memory limits and adjusts runtime memory limit in-place.
type Watcher struct {
	opts       Options
	lastCgroup int64
	mu         sync.Mutex
	stopCh     chan struct{}
}

// New creates a new Watcher instance with sane defaults.
func New(opts Options) *Watcher {
	if opts.CgroupPath == "" {
		opts.CgroupPath = DefaultCgroupPath
	}
	if opts.Ratio <= 0 || opts.Ratio > 1.0 {
		opts.Ratio = DefaultRatio
	}
	if opts.Interval <= 0 {
		opts.Interval = DefaultInterval
	}
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}

	return &Watcher{
		opts:   opts,
		stopCh: make(chan struct{}),
	}
}

// Start begins monitoring the cgroup limit in a background goroutine.
func (w *Watcher) Start(ctx context.Context) error {
	// Initial evaluation
	if err := w.update(); err != nil {
		w.opts.Logger.Warn("inplace-gomemlimit: initial update failed or no cgroup limit", "error", err)
	}

	go w.loop(ctx)
	return nil
}

// Stop terminates the watcher goroutine.
func (w *Watcher) Stop() {
	close(w.stopCh)
}

func (w *Watcher) loop(ctx context.Context) {
	ticker := time.NewTicker(w.opts.Interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-w.stopCh:
			return
		case <-ticker.C:
			_ = w.update()
		}
	}
}

func (w *Watcher) update() error {
	w.mu.Lock()
	defer w.mu.Unlock()

	data, err := os.ReadFile(w.opts.CgroupPath)
	if err != nil {
		return fmt.Errorf("read cgroup file: %w", err)
	}

	raw := strings.TrimSpace(string(data))
	if raw == "max" {
		// No limit enforced by cgroup
		return nil
	}

	maxBytes, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		return fmt.Errorf("parse cgroup memory: %w", err)
	}

	if maxBytes == w.lastCgroup {
		// Limit unchanged
		return nil
	}

	targetLimit := int64(float64(maxBytes) * w.opts.Ratio)
	oldLimit := debug.SetMemoryLimit(targetLimit)
	w.lastCgroup = maxBytes

	w.opts.Logger.Info("inplace-gomemlimit: updated GOMEMLIMIT in-place",
		"cgroup_max_bytes", maxBytes,
		"cgroup_max_human", formatBytes(maxBytes),
		"old_gomemlimit", formatBytes(oldLimit),
		"new_gomemlimit", formatBytes(targetLimit),
		"ratio", w.opts.Ratio,
	)

	if w.opts.OnLimitChanged != nil {
		w.opts.OnLimitChanged(oldLimit, targetLimit, maxBytes)
	}

	return nil
}

func formatBytes(b int64) string {
	if b < 0 {
		return "math.MaxInt64"
	}
	const unit = 1024
	if b < unit {
		return fmt.Sprintf("%d B", b)
	}
	div, exp := int64(unit), 0
	for n := b / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(b)/float64(div), "KMGTPE"[exp])
}
