package logging

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
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
	Updated    int       `json:"updated"` // re-enable выключенных записей
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

// rotators — активные lumberjack-логгеры по пути файла: нужны, чтобы
// «Очистить логи» шла через механизм ротации, а не truncate мимо счётчика.
var (
	rotMu    sync.Mutex
	rotators = map[string]*lumberjack.Logger{}
)

func registerRotator(path string, lj *lumberjack.Logger) {
	rotMu.Lock()
	defer rotMu.Unlock()
	rotators[path] = lj
}

// RotateLogFile ротирует активный лог (текущий уходит в бэкап, начинается
// пустой): состояние lumberjack остаётся консистентным, в отличие от
// прямого truncate файла.
func RotateLogFile(path string) error {
	rotMu.Lock()
	lj := rotators[path]
	rotMu.Unlock()
	if lj == nil {
		return fmt.Errorf("no active rotator for %s", path)
	}
	return lj.Rotate()
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
		registerRotator(cfg.Logging.File, lj)
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
		"updated", result.Updated,
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

// maxTailScan — предел сканирования с конца файла (8 МБ): дальше выборка
// считается усечённой, вместо чтения всего файла в память.
const maxTailScan = 8 << 20

// tailChunk — размер чанка обратного чтения.
const tailChunk = 64 << 10

// TailLogFile читает последние limit строк лог-файла и парсит их как JSON.
func TailLogFile(path string, limit int) ([]map[string]any, error) {
	entries, _, err := TailLogFileFiltered(path, limit, nil)
	return entries, err
}

// TailLogFileFiltered читает файл с конца, возвращая до limit записей,
// удовлетворяющих match (nil — все). Порядок — как в файле (старые первыми).
// truncated=true, если сканирование упёрлось в maxTailScan раньше начала
// файла: при активных фильтрах выборка может быть неполной.
func TailLogFileFiltered(path string, limit int, match func(map[string]any) bool) ([]map[string]any, bool, error) {
	if limit <= 0 {
		limit = 100
	}
	if _, err := os.Stat(path); os.IsNotExist(err) {
		return []map[string]any{}, false, nil
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, false, fmt.Errorf("open log: %w", err)
	}
	defer func() { _ = file.Close() }()

	st, err := file.Stat()
	if err != nil {
		return nil, false, fmt.Errorf("stat log: %w", err)
	}
	size := st.Size()

	start := size - maxTailScan
	truncated := false
	if start < 0 {
		start = 0
	} else {
		truncated = true
	}

	// newest-first: разворачиваем в конце под контракт TailLogFile.
	found := make([]map[string]any, 0, limit)
	accept := func(raw []byte) bool {
		raw = bytes.TrimSpace(raw)
		if len(raw) == 0 {
			return len(found) < limit
		}
		var entry map[string]any
		if err := json.Unmarshal(raw, &entry); err != nil {
			entry = map[string]any{"raw": string(raw)}
		}
		if match != nil && !match(entry) {
			return len(found) < limit
		}
		found = append(found, entry)
		return len(found) < limit
	}

	var pending []byte // неполная строка на границе чанков (голова)
	off := size
	for off > start && len(found) < limit {
		n := int64(tailChunk)
		if n > off-start {
			n = off - start
		}
		buf := make([]byte, n)
		if _, err := file.ReadAt(buf, off-n); err != nil {
			return nil, false, fmt.Errorf("read log: %w", err)
		}
		off -= n

		data := buf
		if len(pending) > 0 {
			data = append(buf, pending...)
			pending = nil
		}
		// data покрывает [off, конец]; segs[0] — оборванная голова,
		// остальные строки полные (завершены \n справа или концом файла).
		segs := bytes.Split(data, []byte{'\n'})
		for i := len(segs) - 1; i >= 1; i-- {
			if !accept(segs[i]) {
				goto done
			}
		}
		if off == start {
			// Дошли до начала сканирования: segs[0] полон только если
			// это начало файла, иначе это оборванный кусок — выбросить.
			if start == 0 {
				accept(segs[0])
			}
		} else {
			pending = segs[0]
		}
	}
done:
	// Разворот в порядок файла (старые первыми).
	for i, j := 0, len(found)-1; i < j; i, j = i+1, j-1 {
		found[i], found[j] = found[j], found[i]
	}
	return found, truncated, nil
}
