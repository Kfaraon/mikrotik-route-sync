package bot

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/Kfaraon/mikrotik-route-sync/internal/config"
	"github.com/Kfaraon/mikrotik-route-sync/internal/core"
	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
	"golang.org/x/time/rate"
)

type State struct{ Selected map[string]bool }
type Bot struct {
	api      *tgbotapi.BotAPI
	cfg      config.Telegram
	syncer   *core.Syncer
	services []string
	log      *slog.Logger
	mu       sync.RWMutex
	states   map[int64]*State
	limits   map[int64]*rate.Limiter
}

func New(c config.Telegram, s *core.Syncer, services []string, log *slog.Logger) (*Bot, error) {
	api, e := tgbotapi.NewBotAPI(c.BotToken)
	if e != nil {
		return nil, e
	}
	return &Bot{api: api, cfg: c, syncer: s, services: services, log: log, states: map[int64]*State{}, limits: map[int64]*rate.Limiter{}}, nil
}
func (b *Bot) authorized(id int64) bool {
	needle := fmt.Sprint(id)
	for _, x := range b.cfg.AuthorizedChatIDs {
		if x == needle {
			return true
		}
	}
	return false
}
func (b *Bot) limiter(id int64) *rate.Limiter {
	b.mu.Lock()
	defer b.mu.Unlock()
	l := b.limits[id]
	if l == nil {
		r := b.cfg.RateLimit
		if r < 1 {
			r = 1
		}
		l = rate.NewLimiter(rate.Limit(r), r)
		b.limits[id] = l
	}
	return l
}
func (b *Bot) menu(id int64) {
	msg := tgbotapi.NewMessage(id, "MikroTik Route Sync\nВыберите действие:")
	msg.ReplyMarkup = tgbotapi.NewInlineKeyboardMarkup(tgbotapi.NewInlineKeyboardRow(tgbotapi.NewInlineKeyboardButtonData("📊 Статус", "status"), tgbotapi.NewInlineKeyboardButtonData("🔄 Синхронизация", "sync:all")))
	_, _ = b.api.Send(msg)
}
func (b *Bot) Run(ctx context.Context) error {
	u := tgbotapi.NewUpdate(0)
	u.Timeout = 30
	updates := b.api.GetUpdatesChan(u)
	for {
		select {
		case <-ctx.Done():
			b.api.StopReceivingUpdates()
			return nil
		case up := <-updates:
			var id int64
			if up.Message != nil {
				id = up.Message.Chat.ID
			} else if up.CallbackQuery != nil {
				id = up.CallbackQuery.Message.Chat.ID
			} else {
				continue
			}
			if !b.authorized(id) {
				b.log.Warn("unauthorized telegram access", "chat_id", id)
				continue
			}
			if !b.limiter(id).Allow() {
				continue
			}
			if up.CallbackQuery != nil {
				_, _ = b.api.Request(tgbotapi.NewCallback(up.CallbackQuery.ID, ""))
				switch up.CallbackQuery.Data {
				case "status":
					b.menu(id)
				case "sync:all":
					go func() {
						c, cancel := context.WithTimeout(ctx, 10*time.Minute)
						defer cancel()
						_ = b.syncer.SyncMany(c, b.services, false)
					}()
				}
				continue
			}
			switch up.Message.Command() {
			case "start", "menu", "status", "help", "whoami":
				b.menu(id)
			case "sync":
				go func() {
					c, cancel := context.WithTimeout(ctx, 10*time.Minute)
					defer cancel()
					_ = b.syncer.SyncMany(c, b.services, false)
				}()
			}
		}
	}
}
