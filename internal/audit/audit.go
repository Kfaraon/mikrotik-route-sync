package audit

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"time"
)

// Action представляет тип действия аудита.
type Action string

const (
	ActionConfigChange Action = "config_change"
	ActionServiceAdd   Action = "service_add"
	ActionServiceDel   Action = "service_delete"
	ActionScheduleChg  Action = "schedule_change"
	ActionSyncStart    Action = "sync_start"
	ActionSyncComplete Action = "sync_complete"
	ActionLogin        Action = "login"
	ActionLogout       Action = "logout"
)

// Entry представляет запись аудита.
type Entry struct {
	Timestamp time.Time         `json:"timestamp"`
	Action    Action            `json:"action"`
	User      string            `json:"user,omitempty"`
	Source    string            `json:"source,omitempty"` // "cli", "web", "telegram", "api"
	Changes   map[string]string `json:"changes,omitempty"`
	Error     string            `json:"error,omitempty"`
	Metadata  map[string]any    `json:"metadata,omitempty"`
}

// Logger предоставляет функционал логирования аудита.
type Logger struct {
	logger *slog.Logger
}

// NewLogger создаёт новый логгер аудита.
func NewLogger(logger *slog.Logger) *Logger {
	return &Logger{logger: logger}
}

// Log записывает событие аудита.
func (l *Logger) Log(entry Entry) {
	if entry.Timestamp.IsZero() {
		entry.Timestamp = time.Now()
	}
	
	// Логируем как структурированное событие
	attrs := []any{
		slog.String("audit_action", string(entry.Action)),
		slog.Time("audit_timestamp", entry.Timestamp),
	}
	
	if entry.User != "" {
		attrs = append(attrs, slog.String("audit_user", entry.User))
	}
	if entry.Source != "" {
		attrs = append(attrs, slog.String("audit_source", entry.Source))
	}
	if entry.Error != "" {
		attrs = append(attrs, slog.String("audit_error", entry.Error))
	}
	
	// Добавляем изменения (без значений секретов)
	if len(entry.Changes) > 0 {
		changesJSON, _ := json.Marshal(entry.Changes)
		attrs = append(attrs, slog.String("audit_changes", string(changesJSON)))
	}
	
	// Добавляем метаданные
	if len(entry.Metadata) > 0 {
		metadataJSON, _ := json.Marshal(entry.Metadata)
		attrs = append(attrs, slog.String("audit_metadata", string(metadataJSON)))
	}
	
	l.logger.Info("audit_event", attrs...)
}

// LogConfigChange логирует изменение конфигурации.
func (l *Logger) LogConfigChange(user, source string, changes map[string]string) {
	l.Log(Entry{
		Action:  ActionConfigChange,
		User:    user,
		Source:  source,
		Changes: changes,
	})
}

// LogServiceAdd логирует добавление сервиса.
func (l *Logger) LogServiceAdd(user, source, service string) {
	l.Log(Entry{
		Action: ActionServiceAdd,
		User:   user,
		Source: source,
		Metadata: map[string]any{
			"service": service,
		},
	})
}

// LogServiceDelete логирует удаление сервиса.
func (l *Logger) LogServiceDelete(user, source, service string) {
	l.Log(Entry{
		Action: ActionServiceDel,
		User:   user,
		Source: source,
		Metadata: map[string]any{
			"service": service,
		},
	})
}

// LogSyncStart логирует начало синхронизации.
func (l *Logger) LogSyncStart(services []string) {
	l.Log(Entry{
		Action: ActionSyncStart,
		Metadata: map[string]any{
			"services": services,
			"count":    len(services),
		},
	})
}

// LogSyncComplete логирует завершение синхронизации.
func (l *Logger) LogSyncComplete(services []string, duration time.Duration, err error) {
	entry := Entry{
		Action: ActionSyncComplete,
		Metadata: map[string]any{
			"services":      services,
			"count":         len(services),
			"duration_ms":   duration.Milliseconds(),
			"duration_human": duration.String(),
		},
	}
	if err != nil {
		entry.Error = err.Error()
	}
	l.Log(entry)
}

// LogLogin логирует успешный вход.
func (l *Logger) LogLogin(user, source string) {
	l.Log(Entry{
		Action: ActionLogin,
		User:   user,
		Source: source,
	})
}

// LogLogout логирует выход.
func (l *Logger) LogLogout(user, source string) {
	l.Log(Entry{
		Action: ActionLogout,
		User:   user,
		Source: source,
	})
}

// MaskSecret маскирует секретное значение для аудита.
// Возвращает только последние 4 символа, остальное заменяет на '*'.
func MaskSecret(value string) string {
	if len(value) <= 4 {
		return "****"
	}
	return fmt.Sprintf("%s%s", maskString(len(value)-4), value[len(value)-4:])
}

func maskString(length int) string {
	result := make([]byte, length)
	for i := range result {
		result[i] = '*'
	}
	return string(result)
}

// ExtractConfigChanges извлекает изменения между двумя конфигурациями.
// Секретные значения маскируются.
func ExtractConfigChanges(oldConfig, newConfig map[string]any, secretKeys []string) map[string]string {
	changes := make(map[string]string)
	
	secretSet := make(map[string]bool)
	for _, key := range secretKeys {
		secretSet[key] = true
	}
	
	// Находим изменённые ключи
	for key, newVal := range newConfig {
		oldVal, exists := oldConfig[key]
		if !exists || fmt.Sprintf("%v", oldVal) != fmt.Sprintf("%v", newVal) {
			// Если это секрет, маскируем значение
			if secretSet[key] {
				changes[key] = "***masked***"
			} else {
				changes[key] = fmt.Sprintf("%v -> %v", oldVal, newVal)
			}
		}
	}
	
	// Находим удалённые ключи
	for key := range oldConfig {
		if _, exists := newConfig[key]; !exists {
			if secretSet[key] {
				changes[key] = "***masked*** (deleted)"
			} else {
				changes[key] = fmt.Sprintf("%v -> (deleted)", oldConfig[key])
			}
		}
	}
	
	return changes
}
