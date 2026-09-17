package logging

import (
	"bufio"
	"context"
	"encoding/json"
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

// SyncResult представляет результат синхронизации для логирования
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

// redactKeys список ключей, значения которых нужно маскировать
var redactKeys = []string{
	"password",
	"token",
	"secret",
	"key",
	"api_key",
	"bot_token",
	"authorization",
	"cookie",
	"session",
}

// isSensitiveKey проверяет, является ли ключ чувствительным
func isSensitiveKey(key string) bool {
	lowerKey := strings.ToLower(key)
	for _, sensitive := range redactKeys {
		if strings.Contains(lowerKey, sensitive) {
			return true
		}
	}
	return false
}

// Setup настраивает глобальное логирование на основе конфигурации
func Setup(cfg *config.Config) (*slog.Logger, error) {
	level := parseLevel(cfg.Logging.Level)

	var writers []io.Writer

	// Настраиваем файл с ротацией
	if cfg.Logging.File != "" {
		logDir := filepath.Dir(cfg.Logging.File)
		if err := os.MkdirAll(logDir, 0o755); err != nil {
			return nil, fmt.Errorf("create log directory: %w", err)
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

	// Вывод в stdout
	if cfg.Logging.AlsoStdout || cfg.Logging.File == "" {
		writers = append(writers, os.Stdout)
	}

	if len(writers) == 0 {
		writers = append(writers, os.Stdout)
	}

	// Объединяем все writers
	multiWriter := io.MultiWriter(writers...)

	// Используем ReplaceAttr для правильного и безопасного redaction в slog
	opts := &slog.HandlerOptions{
		Level: level,
		ReplaceAttr: func(groups []string, a slog.Attr) slog.Attr {
			if isSensitiveKey(a.Key) {
				return slog.String(a.Key, "[REDACTED]")
			}
			return a
		},
	}

	handler := slog.NewJSONHandler(multiWriter, opts)
	logger := slog.New(handler)

	// Устанавливаем глобальный логгер
	slog.SetDefault(logger)

	return logger, nil
}

// New - алиас для совместимости с кодом, где требуется функция New(LoggingConfig)
func New(c config.Logging) *slog.Logger {
	cfg := &config.Config{Logging: c}
	logger, _ := Setup(cfg)
	return logger
}

// parseLevel конвертирует строковый уровень логирования в slog.Level
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

// RedactString маскирует чувствительные данные в строке (для неструктурированных логов или ошибок)
func RedactString(s string) string {
	result := s
	for _, key := range redactKeys {
		result = maskKeyValue(result, key)
	}
	return result
}

// maskKeyValue маскирует значение указанного ключа
func maskKeyValue(s string, key string) string {
	// Маскируем "key":"value" (JSON формат)
	s = strings.ReplaceAll(s, fmt.Sprintf(`"%s":`, key), fmt.Sprintf(`"%s":"[REDACTED]"`, key))

	// Маскируем ключ=значение
	if idx := strings.Index(strings.ToLower(s), key+"="); idx != -1 {
		end := strings.Index(s[idx:], " ")
		if end == -1 {
			end = len(s) - idx
		}
		s = s[:idx] + key + "=[REDACTED]" + s[idx+end:]
	}

	return s
}

// LogSyncResult логирует результат синхронизации одного сервиса
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
		"started_at", result.StartedAt.Format(time.RFC3339),
		"finished_at", result.FinishedAt.Format(time.RFC3339),
	)
}

// LogSyncSummary логирует сводку по нескольким сервисам
func LogSyncSummary(logger *slog.Logger, results []SyncResult) {
	if len(results) == 0 {
		return
	}

	var (
		successCount int
		failedCount  int
		totalAdded   int
		totalRemoved int
	)

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
		"total_services", len(results),
		"success", successCount,
		"failed", failedCount,
		"total_added", totalAdded,
		"total_removed", totalRemoved,
	)

	for _, r := range results {
		if r.Error != "" {
			logger.Warn("service sync failed",
				"service", r.Service,
				"error", r.Error,
			)
		}
	}
}

// TailLogFile читает последние строки лог-файла и парсит их как JSON
func TailLogFile(path string, limit int) ([]map[string]any, error) {
	if limit <= 0 {
		limit = 100
	}

	if _, err := os.Stat(path); os.IsNotExist(err) {
		return []map[string]any{}, nil
	}

	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open log file: %w", err)
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
		return nil, fmt.Errorf("read log file: %w", err)
	}

	start := len(lines) - limit
	if start < 0 {
		start = 0
	}

	var result []map[string]any
	for i := start; i < len(lines); i++ {
		var entry map[string]any
		if err := json.Unmarshal([]byte(lines[i]), &entry); err != nil {
			result = append(result, map[string]any{
				"raw": lines[i],
			})
			continue
		}
		result = append(result, entry)
	}

	return result, nil
}

// GetLogFileSize возвращает размер лог-файла и информацию о ротации
func GetLogFileSize(path string) (map[string]any, error) {
	info := map[string]any{
		"exists": false,
	}

	if path == "" {
		return info, nil
	}

	stat, err := os.Stat(path)
	if os.IsNotExist(err) {
		return info, nil
	}
	if err != nil {
		return nil, fmt.Errorf("stat log file: %w", err)
	}

	info["exists"] = true
	info["size_bytes"] = stat.Size()
	info["size_mb"] = float64(stat.Size()) / (1024 * 1024)
	info["modified_at"] = stat.ModTime().Format(time.RFC3339)

	logDir := filepath.Dir(path)
	logBase := filepath.Base(path)

	entries, err := os.ReadDir(logDir)
	if err != nil {
		return info, nil
	}

	var archiveFiles []string
	var totalArchiveSize int64

	for _, entry := range entries {
		name := entry.Name()
		if strings.HasPrefix(name, logBase) && name != logBase {
			info, err := entry.Info()
			if err != nil {
				continue
			}
			archiveFiles = append(archiveFiles, name)
			totalArchiveSize += info.Size()
		}
	}

	info["archive_count"] = len(archiveFiles)
	info["archive_size_mb"] = float64(totalArchiveSize) / (1024 * 1024)
	info["archive_files"] = archiveFiles

	return info, nil
}

// ClearLogFile очищает лог-файл
func ClearLogFile(path string) error {
	if path == "" {
		return fmt.Errorf("log file path is empty")
	}

	if _, err := os.Stat(path); os.IsNotExist(err) {
		return nil
	}

	return os.WriteFile(path, []byte{}, 0o644)
}

// ValidateLogConfig проверяет конфигурацию логирования
func ValidateLogConfig(cfg *config.LoggingConfig) error {
	if cfg.File != "" {
		dir := filepath.Dir(cfg.File)
		if dir != "." && dir != "" {
			if _, err := os.Stat(dir); os.IsNotExist(err) {
				return fmt.Errorf("log directory %s does not exist", dir)
			}

			info, err := os.Stat(dir)
			if err != nil {
				return fmt.Errorf("cannot stat log directory: %w", err)
			}

			perm := info.Mode().Perm()
			if perm&0o077 != 0 {
				return fmt.Errorf("log directory %s has insecure permissions: %o", dir, perm)
			}
		}
	}

	return nil
}

// LogStartup логирует информацию о запуске приложения
func LogStartup(logger *slog.Logger, version, configPath string) {
	logger.Info("application starting",
		"version", version,
		"config_path", configPath,
		"pid", os.Getpid(),
	)
}

// LogShutdown логирует информацию об остановке приложения
func LogShutdown(logger *slog.Logger, reason string) {
	logger.Info("application shutting down",
		"reason", reason,
	)
}

// LogConfigReload логирует перезагрузку конфигурации
func LogConfigReload(logger *slog.Logger, success bool, err error) {
	if success {
		logger.Info("configuration reloaded successfully")
	} else {
		logger.Error("configuration reload failed", "err", err)
	}
}
