package notifier

import (
	"context"
	"fmt"
	"log/slog"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
	"github.com/Kfaraon/mikrotik-route-sync/internal/config"
)

type SyncResult struct {
	Service  string
	Added    int
	Removed  int
	Errors   int
	DryRun   bool
	Duration string
}

type Notifier interface {
	SyncStart(ctx context.Context, services []string, trigger, schedule string)
	SyncDone(ctx context.Context, results []SyncResult, duration string, dryRun bool)
	Error(ctx context.Context, service string, err error)
	Report(ctx context.Context, cfg *config.Config, text string)
	Test(ctx context.Context) error
}

type Nop struct{}

func (Nop) SyncStart(ctx context.Context, services []string, trigger, schedule string) {}
func (Nop) SyncDone(ctx context.Context, results []SyncResult, duration string, dryRun bool) {}
func (Nop) Error(ctx context.Context, service string, err error) {}
func (Nop) Report(ctx context.Context, cfg *config.Config, text string) {}
func (Nop) Test(ctx context.Context) error { return nil }

type TelegramNotifier struct {
	cfg *config.TelegramConfig
	log *slog.Logger
	api *tgbotapi.BotAPI
}

func FromConfig(cfg config.TelegramConfig, log *slog.Logger) Notifier {
	if !cfg.Enabled || cfg.BotToken == "" {
		return Nop{}
	}

	api, err := tgbotapi.NewBotAPI(cfg.BotToken)
	if err != nil {
		log.Error("telegram notifier init failed", "err", err)
		return Nop{}
	}

	return &TelegramNotifier{
		cfg: &cfg,
		log: log,
		api: api,
	}
}

func (t *TelegramNotifier) send(ctx context.Context, text string) {
	chatID := t.cfg.ChatID
	msg := tgbotapi.NewMessage(0, text)

	if id, err := parseChatID(chatID); err == nil {
		msg.ChatID = id
	} else {
		t.log.Error("invalid chat_id", "chat_id", chatID)
		return
	}

	if _, err := t.api.Send(msg); err != nil {
		t.log.Error("telegram send failed", "err", err)
	}
}

func (t *TelegramNotifier) SyncStart(ctx context.Context, services []string, trigger, schedule string) {
	text := fmt.Sprintf("🔄 *Sync started*\nServices: %s\nTrigger: %s",
		fmt.Sprintf("%v", services), trigger)
	if schedule != "" {
		text += fmt.Sprintf("\nSchedule: %s", schedule)
	}
	t.send(ctx, text)
}

func (t *TelegramNotifier) SyncDone(ctx context.Context, results []SyncResult, duration string, dryRun bool) {
	text := "✅ *Sync completed*\n"
	if dryRun {
		text = "🔍 *Dry-run completed*\n"
	}

	for _, r := range results {
		text += fmt.Sprintf("• %s: +%d/-%d", r.Service, r.Added, r.Removed)
		if r.Errors > 0 {
			text += fmt.Sprintf(" (%d errors)", r.Errors)
		}
		text += "\n"
	}

	text += fmt.Sprintf("\nDuration: %s", duration)
	t.send(ctx, text)
}

func (t *TelegramNotifier) Error(ctx context.Context, service string, err error) {
	text := fmt.Sprintf("❌ *Error*\nService: %s\nError: %s", service, err.Error())
	t.send(ctx, text)
}

func (t *TelegramNotifier) Report(ctx context.Context, cfg *config.Config, text string) {
	t.send(ctx, text)
}

func (t *TelegramNotifier) Test(ctx context.Context) error {
	t.send(ctx, "🧪 Test message from mikrotik-route-sync")
	return nil
}

func parseChatID(s string) (int64, error) {
	var id int64
	_, err := fmt.Sscanf(s, "%d", &id)
	return id, err
}
