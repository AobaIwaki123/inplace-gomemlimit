package main

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"runtime/debug"
	"strings"
	"time"

	_ "github.com/AobaIwaki123/inplace-gomemlimit/auto"
)

type StatusResponse struct {
	Timestamp      time.Time `json:"timestamp"`
	CgroupMaxRaw   string    `json:"cgroup_max_raw"`
	CurrentMemlimit int64     `json:"current_gomemlimit_bytes"`
	CurrentMemlimitHuman string `json:"current_gomemlimit_human"`
}

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	slog.SetDefault(logger)

	port := 8080
	mux := http.NewServeMux()

	mux.HandleFunc("GET /status", func(w http.ResponseWriter, r *http.Request) {
		currentLimit := debug.SetMemoryLimit(-1)
		cgroupRaw := readCgroupMax()

		resp := StatusResponse{
			Timestamp:            time.Now(),
			CgroupMaxRaw:         cgroupRaw,
			CurrentMemlimit:      currentLimit,
			CurrentMemlimitHuman: formatBytes(currentLimit),
		}

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	})

	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})

	go func() {
		// Periodically log memory state
		ticker := time.NewTicker(3 * time.Second)
		for range ticker.C {
			currentLimit := debug.SetMemoryLimit(-1)
			cgroupRaw := readCgroupMax()
			logger.Info("runtime memory status",
				"cgroup_max", cgroupRaw,
				"gomemlimit", formatBytes(currentLimit),
			)
		}
	}()

	addr := fmt.Sprintf(":%d", port)
	logger.Info("starting inplace-gomemlimit demo server", "addr", addr)
	if err := http.ListenAndServe(addr, mux); err != nil {
		logger.Error("server failed", "error", err)
	}
}

func readCgroupMax() string {
	data, err := os.ReadFile("/sys/fs/cgroup/memory.max")
	if err != nil {
		return fmt.Sprintf("error: %v", err)
	}
	return strings.TrimSpace(string(data))
}

func formatBytes(b int64) string {
	if b < 0 {
		return "math.MaxInt64 (unlimited)"
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
