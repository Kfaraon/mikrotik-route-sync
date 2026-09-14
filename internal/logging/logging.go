package logging

import (
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/Kfaraon/mikrotik-route-sync/internal/config"
	"gopkg.in/natefinch/lumberjack.v2"
)

func New(cfg config.LoggingConfig) *slog.Logger {
	var writers []io.Writer
	if cfg.Stdout || cfg.File == "" {
		writers = append(writers, os.Stdout)
	}
	if cfg.File != "" {
		_ = os.MkdirAll(filepath.Dir(cfg.File), 0o755)
		size := max(cfg.MaxSizeMB, 1)
		backups := max(cfg.MaxFiles-1, 1)
		if cfg.MaxTotalMB > 0 {
			byTotal := cfg.MaxTotalMB/size - 1
			if byTotal < 0 {
				byTotal = 0
			}
			if byTotal < backups {
				backups = byTotal
			}
		}
		writers = append(writers, &lumberjack.Logger{Filename: cfg.File, MaxSize: size, MaxBackups: backups, Compress: cfg.Compress})
	}
	level := slog.LevelInfo
	switch strings.ToLower(cfg.Level) {
	case "debug":
		level = slog.LevelDebug
	case "warn", "warning":
		level = slog.LevelWarn
	case "error":
		level = slog.LevelError
	}
	return slog.New(slog.NewJSONHandler(io.MultiWriter(writers...), &slog.HandlerOptions{Level: level}))
}
func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}
