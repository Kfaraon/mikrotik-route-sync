package notifier

import (
	"context"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"github.com/Kfaraon/mikrotik-route-sync/internal/config"
	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
)

type SyncResult struct {
	Service  string        `json:"service"`
	Method   string        `json:"method"`
	Prefixes int           `json:"prefixes"`
	Added    int           `json:"added"`
	Removed  int           `json:"removed"`
	Error    string        `json:"error,omitempty"`
	Duration time.Duration `json:"-"`
}
type Notifier interface {
	SyncStart(context.Context, []string, string, string)
	SyncDone(context.Context, []SyncResult, time.Duration, bool)
	Error(context.Context, string, error)
	Report(context.Context, string)
	Test(context.Context) error
}
type telegram struct {
	cfg    config.TelegramConfig
	log    *slog.Logger
	api    *tgbotapi.BotAPI
	chatID int64
}
type nop struct{}

func FromConfig(cfg config.TelegramConfig, log *slog.Logger) Notifier {
	if !cfg.Enabled || cfg.BotToken == "" {
		return nop{}
	}
	api, err := tgbotapi.NewBotAPI(cfg.BotToken)
	if err != nil {
		log.Error("telegram init failed", "err", err)
		return nop{}
	}
	id, _ := strconv.ParseInt(cfg.ChatID, 10, 64)
	return &telegram{cfg: cfg, log: log, api: api, chatID: id}
}
func (t *telegram) send(ctx context.Context, text string) error {
	if t.chatID == 0 {
		return fmt.Errorf("telegram chat_id is not configured")
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
	}
	m := tgbotapi.NewMessage(t.chatID, text)
	_, err := t.api.Send(m)
	return err
}
func (t *telegram) SyncStart(ctx context.Context, s []string, trigger, schedule string) {
	_ = t.send(ctx, fmt.Sprintf("🔄 Sync started\nServices: %s\nTrigger: %s\nSchedule: %s", strings.Join(s, ", "), trigger, schedule))
}
func (t *telegram) SyncDone(ctx context.Context, r []SyncResult, d time.Duration, dry bool) {
	var b strings.Builder
	fmt.Fprintf(&b, "✅ Sync finished in %s", d.Round(time.Millisecond))
	if dry {
		b.WriteString(" (dry-run)")
	}
	for _, x := range r {
		fmt.Fprintf(&b, "\n• %s: +%d -%d, %d prefixes", x.Service, x.Added, x.Removed, x.Prefixes)
		if x.Error != "" {
			fmt.Fprintf(&b, " ERROR: %s", x.Error)
		}
	}
	_ = t.send(ctx, b.String())
}
func (t *telegram) Error(ctx context.Context, s string, e error) {
	_ = t.send(ctx, fmt.Sprintf("❌ %s: %v", s, e))
}
func (t *telegram) Report(ctx context.Context, s string) { _ = t.send(ctx, s) }
func (t *telegram) Test(ctx context.Context) error {
	return t.send(ctx, "✅ mikrotik-route-sync Telegram test")
}
func (nop) SyncStart(context.Context, []string, string, string)         {}
func (nop) SyncDone(context.Context, []SyncResult, time.Duration, bool) {}
func (nop) Error(context.Context, string, error)                        {}
func (nop) Report(context.Context, string)                              {}
func (nop) Test(context.Context) error                                  { return nil }
