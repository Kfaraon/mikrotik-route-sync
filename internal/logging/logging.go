package logging

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/Kfaraon/mikrotik-route-sync/internal/config"
	"gopkg.in/natefinch/lumberjack.v2"
)

// SyncResult — результат синхронизации для логирования.
type SyncResult struct {
	Service    string    `json:"service"`
	Success    bool      `json:"success"`
	Added      int       `json:"added"`
	Removed    int       `json:"removed"`
	Unchanged  int       `json:"unchanged"`
	Error      string    `json:"error,omitempty"`
	DryRun     bool      `json:"dry_run"`
	StartedAt  time.Time `json:"started_at"`
	FinishedAt time.Time `json:"finished_at"`
	Duration   string    `json:"duration"`
}

// redactKeys — список ключей, значения которых маскируются.
var redactKeys = []string{
	"password", "token", "secret", "key", "api_key",
	"bot_token", "authorization", "cookie", "session",
}

// isSensitiveKey проверяет, является ли ключ чувствительным.
func isSensitiveKey(key string) bool {
	lowerKey := strings.ToLower(key)
	for _, sensitive := range redactKeys {
		if strings.Contains(lowerKey, sensitive) {
			return true
		}
	}
	return false
}

// levelVar — общий уровень логирования: позволяет менять logging.level на лету
// без пересоздания логгеров (SetLevel из web UI при сохранении настроек).
var levelVar = new(slog.LevelVar)

// SetLevel применяет уровень логирования на лету ("debug"|"info"|"warn"|"error").
func SetLevel(level string) {
	levelVar.Set(parseLevel(level))
}

// Setup настраивает глобальное логирование на основе конфигурации.
func Setup(cfg *config.Config) (*slog.Logger, error) {
	levelVar.Set(parseLevel(cfg.Logging.Level))

	var writers []io.Writer

	// Файл с ротацией
	if cfg.Logging.File != "" {
		logDir := filepath.Dir(cfg.Logging.File)
		if err := os.MkdirAll(logDir, 0o700); err != nil {
			return nil, fmt.Errorf("create log dir: %w", err)
		}
		maxSize := cfg.Logging.MaxSizeMB
		if maxSize <= 0 {
			maxSize = 10
		}
		maxFiles := cfg.Logging.MaxFiles
		if maxFiles <= 0 {
			maxFiles = 5
		}
		lj := &lumberjack.Logger{
			Filename:   cfg.Logging.File,
			MaxSize:    maxSize,
			MaxBackups: maxFiles,
			Compress:   cfg.Logging.Compress,
		}
		writers = append(writers, lj)
	}

	// Дублирование в stdout
	if cfg.Logging.AlsoStdout || cfg.Logging.File == "" {
		writers = append(writers, os.Stdout)
	}
	if len(writers) == 0 {
		writers = append(writers, os.Stdout)
	}

	multiWriter := io.MultiWriter(writers...)

	// ReplaceAttr для redaction чувствительных полей
	opts := &slog.HandlerOptions{
		Level: levelVar,
		ReplaceAttr: func(groups []string, a slog.Attr) slog.Attr {
			if isSensitiveKey(a.Key) {
				return slog.String(a.Key, "[REDACTED]")
			}
			return a
		},
	}

	handler := slog.NewJSONHandler(multiWriter, opts)
	logger := slog.New(handler)
	slog.SetDefault(logger)
	return logger, nil
}

// New — алиас для совместимости.
func New(c config.LoggingConfig) *slog.Logger {
	cfg := &config.Config{Logging: c}
	logger, _ := Setup(cfg)
	return logger
}

// parseLevel конвертирует строковый уровень в slog.Level.
func parseLevel(level string) slog.Level {
	switch strings.ToLower(level) {
	case "debug":
		return slog.LevelDebug
	case "info", "":
		return slog.LevelInfo
	case "warn", "warning":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}

// RedactString маскирует чувствительные данные в строке.
func RedactString(s string) string {
	result := s
	for _, key := range redactKeys {
		result = maskKeyValue(result, key)
	}
	return result
}

// RedactError возвращает копию ошибки с замаскированными секретами.
// Используется перед отправкой в Telegram / API (PROMPT XI.7).
func RedactError(err error) error {
	if err == nil {
		return nil
	}
	return errors.New(RedactString(err.Error()))
}

// maskKeyValue маскирует значение указанного ключа в строке.
func maskKeyValue(s string, key string) string {
	s = strings.ReplaceAll(s, fmt.Sprintf(`"%s":`, key), fmt.Sprintf(`"%s":"[REDACTED]"`, key))
	if idx := strings.Index(strings.ToLower(s), key+"="); idx != -1 {
		end := strings.Index(s[idx:], " ")
		if end == -1 {
			end = len(s) - idx
		}
		s = s[:idx] + key + "=[REDACTED]" + s[idx+end:]
	}
	return s
}

// LogSyncResult логирует результат синхронизации одного сервиса.
func LogSyncResult(logger *slog.Logger, result SyncResult, elapsed time.Duration) {
	level := slog.LevelInfo
	msg := "sync completed"
	if result.Error != "" {
		level = slog.LevelError
		msg = "sync failed"
	} else if result.DryRun {
		msg = "dry run completed"
	}
	logger.Log(context.Background(), level, msg,
		"service", result.Service,
		"success", result.Success,
		"added", result.Added,
		"removed", result.Removed,
		"unchanged", result.Unchanged,
		"dry_run", result.DryRun,
		"duration", elapsed.String(),
		"error", result.Error,
	)
}

// LogSyncSummary логирует сводку по нескольким сервисам.
func LogSyncSummary(logger *slog.Logger, results []SyncResult) {
	if len(results) == 0 {
		return
	}
	var successCount, failedCount, totalAdded, totalRemoved int
	for _, r := range results {
		if r.Success {
			successCount++
		} else {
			failedCount++
		}
		totalAdded += r.Added
		totalRemoved += r.Removed
	}
	logger.Info("sync summary",
		"total", len(results),
		"success", successCount,
		"failed", failedCount,
		"added", totalAdded,
		"removed", totalRemoved,
	)
	for _, r := range results {
		if r.Error != "" {
			logger.Warn("sync failed", "service", r.Service, "error", r.Error)
		}
	}
}

// TailLogFile читает последние строки лог-файла и парсит их как JSON.
func TailLogFile(path string, limit int) ([]map[string]any, error) {
	if limit <= 0 {
		limit = 100
	}
	if _, err := os.Stat(path); os.IsNotExist(err) {
		return []map[string]any{}, nil
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open log: %w", err)
	}
	defer file.Close()

	var lines []string
	scanner := bufio.NewScanner(file)
	buf := make([]byte, 0, 64*1024)
	scanner.Buffer(buf, 1024*1024)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line != "" {
			lines = append(lines, line)
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("read log: %w", err)
	}

	start := len(lines) - limit
	if start < 0 {
		start = 0
	}

	var result []map[string]any
	for i := start; i < len(lines); i++ {
		var entry map[string]any
		if err := json.Unmarshal([]byte(lines[i]), &entry); err != nil {
			result = append(result, map[string]any{"raw": lines[i]})
			continue
		}
		result = append(result, entry)
	}
	return result, nil
}
