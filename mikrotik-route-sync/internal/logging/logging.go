package logging

import (
	"io"
	"log/slog"
	"os"
	"github.com/example/mikrotik-route-sync/internal/config"
	"gopkg.in/natefinch/lumberjack.v2"
)

func New(cfg config.LoggingConfig) *slog.Logger {
	lvl := slog.LevelInfo
	switch cfg.Level {
	case "debug": lvl = slog.LevelDebug
	case "warn": lvl = slog.LevelWarn
	case "error": lvl = slog.LevelError
	}
	rotator := &lumberjack.Logger{
		Filename: cfg.File, MaxSize: cfg.MaxSizeMB, MaxBackups: cfg.MaxFiles,
		Compress: cfg.Compress, LocalTime: true,
	}
	var w io.Writer = rotator
	if cfg.AlsoStdout { w = io.MultiWriter(os.Stdout, rotator) }
	return slog.New(slog.NewJSONHandler(w, &slog.HandlerOptions{Level: lvl}))
}