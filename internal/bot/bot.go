package bot

import (
	"context"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"sync"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
	"github.com/Kfaraon/mikrotik-route-sync/internal/config"
	"github.com/Kfaraon/mikrotik-route-sync/internal/core"
)

type state struct {
	screen   string          // "main", "sync_menu", "sync_services", "confirm", "status", "schedules"
	selected map[string]bool
}

type bot struct {
	cfg        *config.Config
	syncer     *core.Syncer
	log        *slog.Logger
	api        *tgbotapi.BotAPI
	authorized map[int64]bool
	mtx        sync.Mutex
	states     map[int64]*state
}

func Run(ctx context.Context, cfg *config.Config, syncer *core.Syncer, log *slog.Logger) error {
	if !cfg.Telegram.Enabled || cfg.Telegram.BotToken == "" {
		log.Info("telegram bot disabled")
		return nil
	}

	api, err := tgbotapi.NewBotAPI(cfg.Telegram.BotToken)
	if err != nil {
		return fmt.Errorf("create bot: %w", err)
	}

	log.Info("telegram bot started", "user", api.Self.UserName)

	b := &bot{
		cfg:        cfg,
		syncer:     syncer,
		log:        log,
		api:        api,
		authorized: map[int64]bool{},
		states:     map[int64]*state{},
	}

	for _, id := range cfg.Telegram.AuthorizedChatIDs {
		if n, err := strconv.ParseInt(id, 10, 64); err == nil {
			b.authorized[n] = true
		}
	}
	if n, err := strconv.ParseInt(cfg.Telegram.ChatID, 10, 64); err == nil {
		b.authorized[n] = true
	}

	u := tgbotapi.NewUpdate(0)
	u.Timeout = 60

	updates := api.GetUpdatesChan(u)

	for {
		select {
		case <-ctx.Done():
			api.StopReceivingUpdates()
			return nil
		case upd := <-updates:
			if upd == nil {
				continue
			}
			b.handle(upd)
		}
	}
}

func (b *bot) handle(upd tgbotapi.Update) {
	if upd.Message != nil {
		chatID := upd.Message.Chat.ID
		if !b.authorized[chatID] {
			return
		}

		switch upd.Message.Command() {
		case "start", "menu":
			b.showMain(chatID)
		case "status":
			b.showStatus(chatID)
		case "sync":
			b.showSyncMenu(chatID)
		case "schedule":
			b.showSchedules(chatID)
		case "help":
			b.sendText(chatID, "/menu — главное меню\n/status — статус сервисов\n/sync — синхронизация\n/schedule — расписания")
		}
		return
	}

	if upd.CallbackQuery != nil {
		b.handleCallback(upd.CallbackQuery)
	}
}

func (b *bot) getState(chatID int64) *state {
	b.mtx.Lock()
	defer b.mtx.Unlock()

	st := b.states[chatID]
	if st == nil {
		st = &state{screen: "main", selected: map[string]bool{}}
		b.states[chatID] = st
	}
	return st
}

func (b *bot) handleCallback(cb *tgbotapi.CallbackQuery) {
	chatID := cb.Message.Chat.ID
	if !b.authorized[chatID] {
		return
	}

	st := b.getState(chatID)
	data := cb.Data

	var resp tgbotapi.EditMessageTextConfig

	switch {
	case data == "main":
		st.screen = "main"
		resp = b.renderMain(chatID, cb.Message.MessageID)

	case data == "sync":
		st.screen = "sync_menu"
		resp = b.renderSyncMenu(chatID, cb.Message.MessageID)

	case data == "status":
		st.screen = "status"
		resp = b.renderStatus(chatID, cb.Message.MessageID)

	case data == "schedule":
		st.screen = "schedules"
		resp = b.renderSchedules(chatID, cb.Message.MessageID)

	case data == "settings":
		resp = b.renderSettings(chatID, cb.Message.MessageID)

	case data == "sync:all":
		st.screen = "confirm"
		st.selected = map[string]bool{}
		for _, s := range b.cfg.Services {
			st.selected[s] = true
		}
		resp = b.renderConfirm(chatID, cb.Message.MessageID, st)

	case data == "sync:services":
		st.screen = "sync_services"
		st.selected = map[string]bool{}
		resp = b.renderServices(chatID, cb.Message.MessageID, st)

	case strings.HasPrefix(data, "toggle:"):
		svc := strings.TrimPrefix(data, "toggle:")
		st.selected[svc] = !st.selected[svc]
		resp = b.renderServices(chatID, cb.Message.MessageID, st)

	case data == "select:all":
		for _, s := range b.cfg.Services {
			st.selected[s] = true
		}
		resp = b.renderServices(chatID, cb.Message.MessageID, st)

	case data == "start:selected":
		var svcs []string
		for s, on := range st.selected {
			if on {
				svcs = append(svcs, s)
			}
		}
		st.screen = "main"

		if len(svcs) == 0 {
			resp = tgbotapi.NewEditMessageText(chatID, cb.Message.MessageID, "❌ Не выбрано ни одного сервиса.")
		} else {
			resp = tgbotapi.NewEditMessageText(chatID, cb.Message.MessageID,
				fmt.Sprintf("▶️ Запущено для: %s", strings.Join(svcs, ", ")))
			go func() {
				ctx := context.Background()
				b.syncer.SyncMany(ctx, svcs, false)
			}()
		}

	case data == "confirm:cancel":
		st.screen = "main"
		st.selected = map[string]bool{}
		resp = b.renderMain(chatID, cb.Message.MessageID)

	default:
		resp = tgbotapi.NewEditMessageText(chatID, cb.Message.MessageID, "Неизвестная команда")
	}

	resp.ChatID = chatID
	if _, err := b.api.Send(resp); err != nil {
		b.log.Error("send callback response", "err", err)
	}

	if _, err := b.api.Request(tgbotapi.NewCallback(cb.ID, "")); err != nil {
		b.log.Error("send callback answer", "err", err)
	}
}

func (b *bot) sendText(chatID int64, text string) {
	msg := tgbotapi.NewMessage(chatID, text)
	if _, err := b.api.Send(msg); err != nil {
		b.log.Error("send text", "err", err)
	}
}

func (b *bot) showMain(chatID int64) {
	msg := tgbotapi.NewMessage(chatID, "🏠 Главное меню")
	msg.ReplyMarkup = mainMenu()
	if _, err := b.api.Send(msg); err != nil {
		b.log.Error("show main", "err", err)
	}
}

func (b *bot) showStatus(chatID int64) {
	ctx := context.Background()
	var lines []string

	for _, svc := range b.cfg.Services {
		routes, err := b.syncer.ListRoutes(ctx, svc)
		if err != nil {
			lines = append(lines, fmt.Sprintf("• %s: ⚠️ ошибка", svc))
			continue
		}
		lines = append(lines, fmt.Sprintf("• %s: %d маршрутов", svc, len(routes)))
	}

	b.sendText(chatID, strings.Join(lines, "\n"))
}

func (b *bot) showSyncMenu(chatID int64) {
	msg := tgbotapi.NewMessage(chatID, "🔄 Что синхронизировать?")
	msg.ReplyMarkup = syncMenu()
	if _, err := b.api.Send(msg); err != nil {
		b.log.Error("show sync menu", "err", err)
	}
}

func (b *bot) showSchedules(chatID int64) {
	var lines []string
	for _, svc := range b.cfg.Services {
		lines = append(lines, fmt.Sprintf("• %s: %s", svc, b.cfg.ScheduleFor(svc)))
	}
	b.sendText(chatID, "📅 Расписания:\n"+strings.Join(lines, "\n"))
}

func (b *bot) renderMain(chatID int64, msgID int) tgbotapi.EditMessageTextConfig {
	c := tgbotapi.NewEditMessageText(chatID, msgID, "🏠 Главное меню")
	m := mainMenu()
	c.ReplyMarkup = &m
	return c
}

func (b *bot) renderSyncMenu(chatID int64, msgID int) tgbotapi.EditMessageTextConfig {
	c := tgbotapi.NewEditMessageText(chatID, msgID, "🔄 Что синхронизировать?")
	m := syncMenu()
	c.ReplyMarkup = &m
	return c
}

func (b *bot) renderStatus(chatID int64, msgID int) tgbotapi.EditMessageTextConfig {
	ctx := context.Background()
	var lines []string

	for _, svc := range b.cfg.Services {
		routes, err := b.syncer.ListRoutes(ctx, svc)
		if err != nil {
			lines = append(lines, fmt.Sprintf("• %s: ⚠️", svc))
			continue
		}
		lines = append(lines, fmt.Sprintf("• %s: %d", svc, len(routes)))
	}

	c := tgbotapi.NewEditMessageText(chatID, msgID, "📊 Статус:\n"+strings.Join(lines, "\n"))
	c.ReplyMarkup = &tgbotapi.InlineKeyboardMarkup{
		InlineKeyboard: [][]tgbotapi.InlineKeyboardButton{
			{
				tgbotapi.NewInlineKeyboardButtonData("🔄 Обновить", "status"),
				tgbotapi.NewInlineKeyboardButtonData("🏠 Меню", "main"),
			},
		},
	}
	return c
}

func (b *bot) renderSchedules(chatID int64, msgID int) tgbotapi.EditMessageTextConfig {
	var lines []string
	for _, svc := range b.cfg.Services {
		lines = append(lines, fmt.Sprintf("• %s: %s", svc, b.cfg.ScheduleFor(svc)))
	}

	c := tgbotapi.NewEditMessageText(chatID, msgID, "📅 Расписания:\n"+strings.Join(lines, "\n"))
	c.ReplyMarkup = &tgbotapi.InlineKeyboardMarkup{
		InlineKeyboard: [][]tgbotapi.InlineKeyboardButton{
			{tgbotapi.NewInlineKeyboardButtonData("🏠 Меню", "main")},
		},
	}
	return c
}

func (b *bot) renderSettings(chatID int64, msgID int) tgbotapi.EditMessageTextConfig {
	text := fmt.Sprintf("⚙️ Настройки:\n• MikroTik: %s\n• Gateway: %s\n• Timezone: %s",
		b.cfg.MikroTik.Host, b.cfg.MikroTik.Gateway, b.cfg.Timezone)

	c := tgbotapi.NewEditMessageText(chatID, msgID, text)
	c.ReplyMarkup = &tgbotapi.InlineKeyboardMarkup{
		InlineKeyboard: [][]tgbotapi.InlineKeyboardButton{
			{tgbotapi.NewInlineKeyboardButtonData("🏠 Меню", "main")},
		},
	}
	return c
}

func (b *bot) renderServices(chatID int64, msgID int, st *state) tgbotapi.EditMessageTextConfig {
	var rows [][]tgbotapi.InlineKeyboardButton

	for _, s := range b.cfg.Services {
		mark := "☐"
		if st.selected[s] {
			mark = "☑"
		}
		rows = append(rows, tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData(fmt.Sprintf("%s %s", mark, s), "toggle:"+s),
		))
	}

	rows = append(rows,
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("▶️ Запустить", "start:selected"),
			tgbotapi.NewInlineKeyboardButtonData("✅ Все", "select:all"),
		),
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("🔙 Назад", "main"),
		),
	)

	c := tgbotapi.NewEditMessageText(chatID, msgID, "Выберите сервисы:")
	c.ReplyMarkup = &tgbotapi.InlineKeyboardMarkup{InlineKeyboard: rows}
	return c
}

func (b *bot) renderConfirm(chatID int64, msgID int, st *state) tgbotapi.EditMessageTextConfig {
	var svcs []string
	for s, on := range st.selected {
		if on {
			svcs = append(svcs, s)
		}
	}

	c := tgbotapi.NewEditMessageText(chatID, msgID, "Запустить: "+strings.Join(svcs, ", "))
	c.ReplyMarkup = &tgbotapi.InlineKeyboardMarkup{
		InlineKeyboard: [][]tgbotapi.InlineKeyboardButton{
			{
				tgbotapi.NewInlineKeyboardButtonData("✅ Подтвердить", "start:selected"),
				tgbotapi.NewInlineKeyboardButtonData("❌ Отмена", "confirm:cancel"),
			},
		},
	}
	return c
}

func mainMenu() tgbotapi.InlineKeyboardMarkup {
	return tgbotapi.NewInlineKeyboardMarkup(
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("📊 Статус", "status"),
			tgbotapi.NewInlineKeyboardButtonData("🔄 Синхронизация", "sync"),
		),
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("📅 Расписания", "schedule"),
			tgbotapi.NewInlineKeyboardButtonData("⚙️ Настройки", "settings"),
		),
	)
}

func syncMenu() tgbotapi.InlineKeyboardMarkup {
	return tgbotapi.NewInlineKeyboardMarkup(
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("✅ Все сервисы", "sync:all"),
		),
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("🎯 По сервисам", "sync:services"),
		),
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("🔙 Назад", "main"),
		),
	)
}
