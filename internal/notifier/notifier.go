package notifier

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/Kfaraon/mikrotik-route-sync/internal/config"
	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
)

type Notifier interface {
	Send(context.Context, string) error
}
type nop struct{}

func (nop) Send(context.Context, string) error { return nil }

type Telegram struct {
	bot  *tgbotapi.BotAPI
	chat int64
	log  *slog.Logger
}

func FromConfig(c config.Telegram, log *slog.Logger) Notifier {
	if !c.Enabled || c.BotToken == "" || c.ChatID == "" {
		return nop{}
	}
	b, e := tgbotapi.NewBotAPI(c.BotToken)
	if e != nil {
		log.Warn("telegram disabled", "error", e)
		return nop{}
	}
	var id int64
	fmt.Sscan(c.ChatID, &id)
	return &Telegram{bot: b, chat: id, log: log}
}
func (t *Telegram) Send(_ context.Context, msg string) error {
	_, e := t.bot.Send(tgbotapi.NewMessage(t.chat, msg))
	return e
}
