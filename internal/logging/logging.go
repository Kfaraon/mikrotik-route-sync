package logging

import (
	"io"
	"log/slog"
	"os"

	"github.com/Kfaraon/mikrotik-route-sync/internal/config"
	"gopkg.in/natefinch/lumberjack.v2"
)

func New(c config.Logging) *slog.Logger {
	var writers []io.Writer
	if c.AlsoStdout {
		writers = append(writers, os.Stdout)
	}
	if c.File != "" {
		writers = append(writers, &lumberjack.Logger{Filename: c.File, MaxSize: c.MaxSizeMB, MaxBackups: c.MaxFiles, Compress: c.Compress})
	}
	w := io.Writer(os.Stdout)
	if len(writers) > 0 {
		w = io.MultiWriter(writers...)
	}
	level := slog.LevelInfo
	switch c.Level {
	case "debug":
		level = slog.LevelDebug
	case "warn":
		level = slog.LevelWarn
	case "error":
		level = slog.LevelError
	}
	return slog.New(slog.NewJSONHandler(w, &slog.HandlerOptions{Level: level}))
}
