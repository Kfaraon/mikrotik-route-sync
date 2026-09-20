// Package bot — компактный Telegram-бот управления синхронизацией (PROMPT V).
//
// Авторизация — ТОЛЬКО по authorized_chat_ids; неавторизованным не отвечаем
// (чтобы не раскрывать существование бота), логируем с WARN.
package bot

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/Kfaraon/mikrotik-route-sync/internal/config"
	"github.com/Kfaraon/mikrotik-route-sync/internal/core"
	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
	"golang.org/x/time/rate"
)

// Bot — Telegram-бот.
type Bot struct {
	api    *tgbotapi.BotAPI
	cfg    config.TelegramConfig
	syncer *core.Syncer
	log    *slog.Logger
	mu     sync.RWMutex
	limits map[int64]*rate.Limiter
}

// New создаёт бота с явными зависимостями.
func New(c config.TelegramConfig, s *core.Syncer, log *slog.Logger) (*Bot, error) {
	if !c.Enabled || c.BotToken == "" {
		return nil, fmt.Errorf("telegram is disabled or bot_token is empty")
	}
	api, err := tgbotapi.NewBotAPI(c.BotToken)
	if err != nil {
		return nil, fmt.Errorf("telegram bot init: %w", err)
	}
	return &Bot{
		api: api, cfg: c, syncer: s, log: log,
		limits: map[int64]*rate.Limiter{},
	}, nil
}

// NewBot — конструктор для CLI: берёт настройки из всего конфига.
func NewBot(cfg *config.Config, s *core.Syncer) (*Bot, error) {
	return New(cfg.Telegram, s, slog.Default())
}

// authorized проверяет chat_id по white-списку.
func (b *Bot) authorized(id int64) bool {
	needle := fmt.Sprint(id)
	for _, x := range b.cfg.AuthorizedChatIDs {
		if x == needle {
			return true
		}
	}
	return false
}

// limiter — rate limit на chat_id (PROMPT V.2).
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

func (b *Bot) send(id int64, text string) {
	msg := tgbotapi.NewMessage(id, text)
	_, _ = b.api.Send(msg)
}

func (b *Bot) menu(id int64) {
	rows := [][]tgbotapi.InlineKeyboardButton{
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("📊 Status", "cmd:status"),
			tgbotapi.NewInlineKeyboardButtonData("🔄 Sync все", "sync:all"),
		),
	}
	for _, svc := range b.syncer.ListServices() {
		rows = append(rows, tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("🔄 "+svc, "sync:"+svc),
		))
	}
	msg := tgbotapi.NewMessage(id,
		"MikroTik Route Sync\n\nКоманды: /status /sync /schedule /help")
	msg.ReplyMarkup = tgbotapi.NewInlineKeyboardMarkup(rows...)
	_, _ = b.api.Send(msg)
}

func (b *Bot) statusText(ctx context.Context) string {
	var sb strings.Builder
	sb.WriteString("Статус:\n")
	sb.WriteString(fmt.Sprintf("  uptime: %s\n", time.Since(b.syncer.StartTime()).Truncate(time.Second)))
	sb.WriteString(fmt.Sprintf("  last sync: %s\n", b.syncer.LastSync().Format(time.RFC3339)))
	sb.WriteString(fmt.Sprintf("  version: %s\n", b.syncer.Version()))
	if dg := b.syncer.DegradedServices(); len(dg) > 0 {
		sb.WriteString("  DEGRADED: " + strings.Join(dg, ", ") + "\n")
	}
	sb.WriteString("Сервисы:\n")
	for _, svc := range b.syncer.ListServices() {
		info, err := b.syncer.InfoService(ctx, svc)
		if err != nil {
			sb.WriteString(fmt.Sprintf("  - %s: error (%v)\n", svc, err))
			continue
		}
		sb.WriteString(fmt.Sprintf("  - %s: routes=%d schedule=%s\n",
			svc, info.RouteCount, info.Schedule))
	}
	return sb.String()
}

func (b *Bot) schedulesText() string {
	var sb strings.Builder
	sb.WriteString("Эффективные расписания:\n")
	for _, svc := range b.syncer.ListServices() {
		sb.WriteString(fmt.Sprintf("  - %s → %s\n", svc, b.syncer.Config().EffectiveSchedule(svc)))
	}
	return sb.String()
}

// startSync запускает синхронизацию в фоне (уведомления шлёт SyncService).
func (b *Bot) startSync(ctx context.Context, services []string) {
	go func() {
		sctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Minute)
		defer cancel()
		_ = b.syncer.SyncMany(sctx, services, false, false)
	}()
}

// Run обрабатывает обновления до отмены ctx.
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
			switch {
			case up.Message != nil:
				id = up.Message.Chat.ID
			case up.CallbackQuery != nil:
				id = up.CallbackQuery.Message.Chat.ID
			default:
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
				b.handleCallback(ctx, id, up.CallbackQuery.Data)
				continue
			}

			switch up.Message.Command() {
			case "start", "menu":
				b.menu(id)
			case "status":
				b.send(id, b.statusText(ctx))
			case "sync":
				b.send(id, "Запускаю синхронизацию всех сервисов…")
				b.startSync(ctx, b.syncer.ListServices())
			case "schedule":
				b.send(id, b.schedulesText())
			case "help":
				b.send(id, "/start /menu — меню\n/status — статус\n/sync — синхронизация\n/schedule — расписания")
			}
		}
	}
}

// handleCallback — inline-кнопки.
func (b *Bot) handleCallback(ctx context.Context, id int64, data string) {
	action, arg, _ := strings.Cut(data, ":")
	switch action {
	case "cmd":
		if arg == "status" {
			b.menu(id)
		}
	case "sync":
		if arg == "all" {
			b.send(id, "Запускаю синхронизацию всех сервисов…")
			b.startSync(ctx, b.syncer.ListServices())
			return
		}
		b.send(id, "Запускаю синхронизацию "+arg+"…")
		b.startSync(ctx, []string{arg})
	}
}

// Start — алиас Run для совместимости с CLI.
func (b *Bot) Start(ctx context.Context) error { return b.Run(ctx) }
