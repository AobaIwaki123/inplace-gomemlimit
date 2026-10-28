package watcher

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func TestWatcher_InPlaceUpdate(t *testing.T) {
	tmpDir := t.TempDir()
	cgroupFile := filepath.Join(tmpDir, "memory.max")

	// 1. Initial 500MiB
	initialBytes := int64(524288000) // 500MiB
	if err := os.WriteFile(cgroupFile, []byte("524288000\n"), 0644); err != nil {
		t.Fatalf("failed to write cgroup file: %v", err)
	}

	var mu sync.Mutex
	var changedCount int
	var recordedNewLimit int64

	w := New(Options{
		CgroupPath: cgroupFile,
		Ratio:      0.90,
		Interval:   20 * time.Millisecond,
		OnLimitChanged: func(oldLimit, newLimit int64, cgroupMax int64) {
			mu.Lock()
			defer mu.Unlock()
			changedCount++
			recordedNewLimit = newLimit
		},
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := w.Start(ctx); err != nil {
		t.Fatalf("failed to start watcher: %v", err)
	}

	// Wait for initial update
	time.Sleep(50 * time.Millisecond)

	mu.Lock()
	currentLimit := recordedNewLimit
	mu.Unlock()

	expectedInitialLimit := int64(float64(initialBytes) * 0.90)
	if currentLimit != expectedInitialLimit {
		t.Errorf("expected initial limit %d, got %d", expectedInitialLimit, currentLimit)
	}

	// 2. Simulate Kubernetes In-Place Resize to 1GiB (1073741824)
	resizedBytes := int64(1073741824)
	if err := os.WriteFile(cgroupFile, []byte("1073741824\n"), 0644); err != nil {
		t.Fatalf("failed to update cgroup file: %v", err)
	}

	// Wait for watcher loop to detect change
	time.Sleep(100 * time.Millisecond)

	mu.Lock()
	currentLimit = recordedNewLimit
	currentCount := changedCount
	mu.Unlock()

	expectedResizedLimit := int64(float64(resizedBytes) * 0.90)
	if currentLimit != expectedResizedLimit {
		t.Errorf("expected resized limit %d, got %d", expectedResizedLimit, currentLimit)
	}

	if currentCount != 2 {
		t.Errorf("expected 2 changes, got %d", currentCount)
	}

	w.Stop()
}
