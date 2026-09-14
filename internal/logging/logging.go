package logging

import (
	"io"
	"log/slog"
	"os"

	"gopkg.in/natefinch/lumberjack.v2"
	"github.com/Kfaraon/mikrotik-route-sync/internal/config"
)

func New(cfg config.LoggingConfig) *slog.Logger {
	var level slog.Level
	switch cfg.Level {
	case "debug":
		level = slog.LevelDebug
	case "info":
		level = slog.LevelInfo
	case "warn":
		level = slog.LevelWarn
	case "error":
		level = slog.LevelError
	default:
		level = slog.LevelInfo
	}

	var writers []io.Writer

	if cfg.AlsoStdout {
		writers = append(writers, os.Stdout)
	}

	if cfg.File != "" {
		lj := &lumberjack.Logger{
			Filename:   cfg.File,
			MaxSize:    cfg.MaxSizeMB,
			MaxBackups: cfg.MaxFiles,
			MaxAge:     30,
			Compress:   cfg.Compress,
		}
		writers = append(writers, lj)
	}

	if len(writers) == 0 {
		writers = append(writers, os.Stdout)
	}

	w := io.MultiWriter(writers...)

	handler := slog.NewJSONHandler(w, &slog.HandlerOptions{Level: level})
	return slog.New(handler)
}
