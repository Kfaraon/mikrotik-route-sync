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
	"github.com/Kfaraon/mikrotik-route-sync/internal/notifier"
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
	wg     sync.WaitGroup
	// syncWg — фоновые синки отдельно от цикла Run: рестарт/остановка
	// не должны висеть до конца 10-минутного синка (см. историю).
	syncWg sync.WaitGroup
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
	// Telegram режет сообщения длиннее 4096 символов — шлём чанками.
	for _, part := range splitMessage(text, 4000) {
		msg := tgbotapi.NewMessage(id, part)
		if _, err := b.api.Send(msg); err != nil {
			b.log.Warn("не удалось отправить в telegram", "chat_id", id, "err", err)
			return
		}
	}
}

// splitMessage режет текст на чанки по рунам, не разрывая UTF-8.
func splitMessage(text string, chunk int) []string {
	if chunk <= 0 {
		return []string{text}
	}
	var out []string
	var cur []rune
	for _, r := range text {
		cur = append(cur, r)
		if len(cur) >= chunk {
			out = append(out, string(cur))
			cur = nil
		}
	}
	if len(cur) > 0 {
		out = append(out, string(cur))
	}
	if len(out) == 0 {
		return []string{""}
	}
	return out
}

func (b *Bot) menu(id int64) {
	rows := [][]tgbotapi.InlineKeyboardButton{
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("📊 Статус", "cmd:status"),
			tgbotapi.NewInlineKeyboardButtonData("🔄 Синхр. все", "sync:all"),
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
	if _, err := b.api.Send(msg); err != nil {
		b.log.Warn("не удалось отправить в telegram", "chat_id", id, "err", err)
	}
}

func (b *Bot) statusText(ctx context.Context) string {
	// Прямые чтения конфига — коротким снапшотом; InfoService/ListServices
	// самодостаточны (секции внутри), сеть — без блокировок.
	b.syncer.RLockConfig()
	fwList := b.syncer.Config().Firewall.AddressList
	timezone := b.syncer.Config().Timezone
	b.syncer.RUnlockConfig()
	var sb strings.Builder
	sb.WriteString("Статус (Firewall Address List):\n")
	fmt.Fprintf(&sb, "  список: %s\n", fwList)
	fmt.Fprintf(&sb, "  аптайм: %s\n", notifier.FormatDur(time.Since(b.syncer.StartTime())))
	last := b.syncer.LastSync()
	if last.IsZero() {
		sb.WriteString("  последняя синхронизация: ещё не выполнялась\n")
	} else {
		loc := notifier.LoadLocation(timezone)
		fmt.Fprintf(&sb, "  последняя синхронизация: %s\n",
			last.In(loc).Format("02.01.2006 15:04:05"))
	}
	fmt.Fprintf(&sb, "  версия: %s\n", b.syncer.Version())
	if dg := b.syncer.DegradedServices(); len(dg) > 0 {
		sb.WriteString("  ⚠️ требует внимания (DEGRADED): " + strings.Join(dg, ", ") + "\n")
	}
	sb.WriteString("Сервисы:\n")
	for _, svc := range b.syncer.ListServices() {
		info, err := b.syncer.InfoService(ctx, svc)
		if err != nil {
			fmt.Fprintf(&sb, "  - %s: ошибка (%s)\n", svc, notifier.LocalizeError(err.Error()))
			continue
		}
		fmt.Fprintf(&sb, "  - %s: записей=%d (comment=%s), расписание=%s\n",
			svc, info.EntryCount, info.Comment, info.Schedule)
	}
	return sb.String()
}

func (b *Bot) schedulesText() string {
	// Без внешнего RLock: ListServices/ServiceSchedules самодостаточны,
	// вложенный захват под ожидающим писателем — дедлок (см. историю).
	scheds := b.syncer.ServiceSchedules()
	var sb strings.Builder
	sb.WriteString("Эффективные расписания:\n")
	for _, svc := range b.syncer.ListServices() {
		fmt.Fprintf(&sb, "  - %s → %s\n", svc, scheds[svc])
	}
	return sb.String()
}

// startSync запускает синхронизацию в фоне (уведомления шлёт SyncService).
// Учёт в отдельном syncWg: Wait() ждёт только цикл Run, рестарт бота
// из веб-запроса не висит до конца синка.
func (b *Bot) startSync(ctx context.Context, services []string) {
	b.syncWg.Add(1)
	go func() {
		defer b.syncWg.Done()
		sctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Minute)
		defer cancel()
		_ = b.syncer.SyncMany(sctx, services, false, false)
	}()
}

// Run обрабатывает обновления до отмены ctx.
func (b *Bot) Run(ctx context.Context) error {
	b.wg.Add(1)
	defer b.wg.Done()
	return b.runLoop(ctx)
}

// GoRun запускает цикл в фоне: Add до go — иначе Wait гоняет с Add
// (restartTelegramBot ждёт сразу после старта).
func (b *Bot) GoRun(ctx context.Context) {
	b.wg.Add(1)
	go func() {
		defer b.wg.Done()
		if err := b.runLoop(ctx); err != nil {
			b.log.Error("telegram-бот остановлен с ошибкой", "err", err)
		}
	}()
}

func (b *Bot) runLoop(ctx context.Context) error {

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
			case up.CallbackQuery != nil && up.CallbackQuery.Message != nil:
				// Message бывает nil (старое/inline-сообщение) — без проверки паника.
				id = up.CallbackQuery.Message.Chat.ID
			default:
				continue
			}

			if !b.authorized(id) {
				b.log.Warn("неавторизованный доступ к telegram", "chat_id", id)
				continue
			}
			if !b.limiter(id).Allow() {
				continue
			}

			if up.CallbackQuery != nil {
				if _, err := b.api.Request(tgbotapi.NewCallback(up.CallbackQuery.ID, "")); err != nil {
					b.log.Warn("не удалось ответить на callback telegram", "chat_id", id, "err", err)
				}
				b.handleCallback(ctx, id, up.CallbackQuery.Data)
				continue
			}

			if up.Message == nil {
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
			b.send(id, b.statusText(ctx))
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

// Wait блокируется до завершения цикла Run (после отмены ctx).
// Фоновые синки идут отдельным syncWg и не задерживают рестарт/остановку.
func (b *Bot) Wait() {
	b.wg.Wait()
}
