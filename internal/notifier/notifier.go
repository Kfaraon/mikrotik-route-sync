package notifier

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/Kfaraon/mikrotik-route-sync/internal/config"
	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
)

// SyncResult — результат синхронизации одного сервиса.
type SyncResult struct {
	Service    string `json:"service"`
	Success    bool   `json:"success"`
	Added      int    `json:"added"`
	Removed    int    `json:"removed"`
	Unchanged  int    `json:"unchanged"`
	Error      string `json:"error,omitempty"`
	DurationMs int64  `json:"duration_ms"`
}

// Notifier — интерфейс для отправки уведомлений (Telegram и др.).
type Notifier interface {
	Send(ctx context.Context, message string) error
	SyncStart(ctx context.Context, services []string, trigger, spec string) error
	SyncDone(ctx context.Context, results []SyncResult, elapsed any, dryRun bool) error
	Error(ctx context.Context, service string, err error) error
}

// TelegramNotifier — реализация Notifier через Telegram.
type TelegramNotifier struct {
	api    *tgbotapi.BotAPI
	chatID int64
	log    *slog.Logger
}

// NewTelegram создаёт Telegram-нотификатор.
func NewTelegram(cfg config.TelegramConfig, log *slog.Logger) (Notifier, error) {
	if !cfg.Enabled || cfg.BotToken == "" {
		return &NoopNotifier{}, nil
	}
	api, err := tgbotapi.NewBotAPI(cfg.BotToken)
	if err != nil {
		return nil, fmt.Errorf("telegram bot init: %w", err)
	}
	var chatID int64
	if cfg.ChatID != "" {
		_, _ = fmt.Sscanf(cfg.ChatID, "%d", &chatID)
	}
	return &TelegramNotifier{api: api, chatID: chatID, log: log}, nil
}

// FromConfig создаёт нотификатор из конфигурации; при ошибке или
// отключённом Telegram возвращает NoopNotifier (работа продолжается).
func FromConfig(cfg config.TelegramConfig, log *slog.Logger) Notifier {
	n, err := NewTelegram(cfg, log)
	if err != nil {
		if log != nil {
			log.Error("telegram notifier init failed, notifications disabled", "err", err)
		}
		return &NoopNotifier{}
	}
	return n
}

func (n *TelegramNotifier) Send(ctx context.Context, message string) error {
	if n.chatID == 0 {
		return nil
	}
	msg := tgbotapi.NewMessage(n.chatID, message)
	msg.ParseMode = tgbotapi.ModeMarkdown
	_, err := n.api.Send(msg)
	return err
}

func (n *TelegramNotifier) SyncStart(ctx context.Context, services []string, trigger, spec string) error {
	return n.Send(ctx, fmt.Sprintf("▶️ Запуск синхронизации: %s\nИсточник: %s, расписание: %s",
		strings.Join(services, ", "), trigger, spec))
}

func (n *TelegramNotifier) SyncDone(ctx context.Context, results []SyncResult, elapsed any, dryRun bool) error {
	msg := "✅ Синхронизация завершена:\n"
	if dryRun {
		msg = "🔎 Пробный запуск (dry-run), изменения не применялись:\n"
	}
	for _, r := range results {
		if r.Success {
			msg += fmt.Sprintf("• %s: + %d добавлено, − %d удалено, %d без изменений\n",
				r.Service, r.Added, r.Removed, r.Unchanged)
		} else {
			reason := r.Error
			if reason == "" {
				reason = "неизвестная ошибка"
			}
			msg += fmt.Sprintf("• %s: ОШИБКА — %s\n", r.Service, reason)
		}
	}
	if sp, ok := elapsed.(time.Duration); ok && sp > 0 {
		msg += fmt.Sprintf("Время выполнения: %s", sp.Round(100*time.Millisecond))
	}
	return n.Send(ctx, msg)
}

func (n *TelegramNotifier) Error(ctx context.Context, service string, err error) error {
	return n.Send(ctx, fmt.Sprintf("⛔ Ошибка синхронизации «%s»: %v", service, err))
}

// NoopNotifier — пустой нотификатор (когда Telegram отключён).
type NoopNotifier struct{}

func (n *NoopNotifier) Send(ctx context.Context, message string) error { return nil }
func (n *NoopNotifier) SyncStart(ctx context.Context, services []string, trigger, spec string) error {
	return nil
}
func (n *NoopNotifier) SyncDone(ctx context.Context, results []SyncResult, elapsed any, dryRun bool) error {
	return nil
}
func (n *NoopNotifier) Error(ctx context.Context, service string, err error) error { return nil }
