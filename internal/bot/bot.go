package bot

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/Kfaraon/mikrotik-route-sync/internal/config"
	"github.com/Kfaraon/mikrotik-route-sync/internal/core"
	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
)

type state struct{ selected map[string]bool }
type Bot struct {
	cfg    *config.Config
	syncer *core.Syncer
	log    *slog.Logger
	api    *tgbotapi.BotAPI
	auth   map[int64]bool
	mu     sync.Mutex
	states map[int64]*state
}

func Run(ctx context.Context, cfg *config.Config, syncer *core.Syncer, log *slog.Logger) error {
	if !cfg.Telegram.Enabled {
		return nil
	}
	api, err := tgbotapi.NewBotAPI(cfg.Telegram.BotToken)
	if err != nil {
		return err
	}
	b := &Bot{cfg: cfg, syncer: syncer, log: log, api: api, auth: map[int64]bool{}, states: map[int64]*state{}}
	for _, v := range append(cfg.Telegram.AuthorizedChatIDs, cfg.Telegram.ChatID) {
		id, _ := strconv.ParseInt(v, 10, 64)
		if id != 0 {
			b.auth[id] = true
		}
	}
	u := tgbotapi.NewUpdate(0)
	u.Timeout = 30
	ch := api.GetUpdatesChan(u)
	for {
		select {
		case <-ctx.Done():
			api.StopReceivingUpdates()
			return nil
		case up := <-ch:
			b.handle(up)
		}
	}
}
func (b *Bot) handle(up tgbotapi.Update) {
	if up.Message != nil {
		if !b.auth[up.Message.Chat.ID] {
			return
		}
		switch up.Message.Command() {
		case "start", "menu":
			b.sendMenu(up.Message.Chat.ID)
		case "status":
			b.sendStatus(up.Message.Chat.ID)
		case "sync":
			b.sendSync(up.Message.Chat.ID)
		case "schedule":
			b.sendSchedules(up.Message.Chat.ID)
		case "help":
			b.send(up.Message.Chat.ID, "/menu /status /sync /schedule /help")
		}
		return
	}
	if up.CallbackQuery != nil {
		b.callback(up.CallbackQuery)
	}
}
func (b *Bot) st(id int64) *state {
	b.mu.Lock()
	defer b.mu.Unlock()
	s := b.states[id]
	if s == nil {
		s = &state{selected: map[string]bool{}}
		b.states[id] = s
	}
	return s
}
func (b *Bot) callback(cb *tgbotapi.CallbackQuery) {
	id := cb.Message.Chat.ID
	if !b.auth[id] {
		return
	}
	data := cb.Data
	s := b.st(id)
	switch {
	case data == "status":
		b.sendStatus(id)
		return
	case data == "sync":
		b.edit(cb, "Синхронизация", syncKB())
	case data == "schedule":
		b.sendSchedules(id)
		return
	case data == "settings":
		b.edit(cb, "Настройки доступны в Web UI и CLI config.", mainKB())
	case data == "back:main":
		b.edit(cb, "Главное меню", mainKB())
	case data == "sync:all":
		s.selected = map[string]bool{}
		for _, v := range b.cfg.Services {
			s.selected[v] = true
		}
		b.edit(cb, "Подтвердить синхронизацию всех сервисов?", confirmKB())
	case data == "sync:services":
		s.selected = map[string]bool{}
		b.edit(cb, "Выберите сервисы", b.servicesKB(s))
	case data == "select:all":
		for _, v := range b.cfg.Services {
			s.selected[v] = true
		}
		b.edit(cb, "Выберите сервисы", b.servicesKB(s))
	case strings.HasPrefix(data, "toggle:"):
		v := strings.TrimPrefix(data, "toggle:")
		s.selected[v] = !s.selected[v]
		b.edit(cb, "Выберите сервисы", b.servicesKB(s))
	case data == "start:selected":
		var list []string
		for v, on := range s.selected {
			if on {
				list = append(list, v)
			}
		}
		sort.Strings(list)
		if len(list) == 0 {
			b.edit(cb, "Ничего не выбрано", b.servicesKB(s))
			break
		}
		b.edit(cb, "▶️ Запущено: "+strings.Join(list, ", "), mainKB())
		go func() {
			if err := b.syncer.SyncMany(context.Background(), list, false); err != nil {
				b.log.Error("bot sync failed", "err", err)
			}
		}()
	}
	_, _ = b.api.Request(tgbotapi.NewCallback(cb.ID, ""))
}
func (b *Bot) sendMenu(id int64) {
	m := tgbotapi.NewMessage(id, "Главное меню")
	m.ReplyMarkup = mainKB()
	_, _ = b.api.Send(m)
}
func (b *Bot) sendSync(id int64) {
	m := tgbotapi.NewMessage(id, "Синхронизация")
	m.ReplyMarkup = syncKB()
	_, _ = b.api.Send(m)
}
func (b *Bot) sendStatus(id int64) {
	st := b.syncer.Status(context.Background())
	b.send(id, fmt.Sprintf("MikroTik: %v\nServices: %d", st["mikrotik"], len(b.cfg.Services)))
}
func (b *Bot) sendSchedules(id int64) {
	var x []string
	for _, s := range b.cfg.Services {
		x = append(x, fmt.Sprintf("• %s: %s", s, b.cfg.ScheduleFor(s)))
	}
	b.send(id, strings.Join(x, "\n"))
}
func (b *Bot) send(id int64, text string) { _, _ = b.api.Send(tgbotapi.NewMessage(id, text)) }
func (b *Bot) edit(cb *tgbotapi.CallbackQuery, text string, kb tgbotapi.InlineKeyboardMarkup) {
	m := tgbotapi.NewEditMessageText(cb.Message.Chat.ID, cb.Message.MessageID, text)
	m.ReplyMarkup = &kb
	_, _ = b.api.Send(m)
}
func mainKB() tgbotapi.InlineKeyboardMarkup {
	return tgbotapi.NewInlineKeyboardMarkup(tgbotapi.NewInlineKeyboardRow(tgbotapi.NewInlineKeyboardButtonData("📊 Статус", "status"), tgbotapi.NewInlineKeyboardButtonData("🔄 Синхронизация", "sync")), tgbotapi.NewInlineKeyboardRow(tgbotapi.NewInlineKeyboardButtonData("📅 Расписания", "schedule"), tgbotapi.NewInlineKeyboardButtonData("⚙️ Настройки", "settings")))
}
func syncKB() tgbotapi.InlineKeyboardMarkup {
	return tgbotapi.NewInlineKeyboardMarkup(tgbotapi.NewInlineKeyboardRow(tgbotapi.NewInlineKeyboardButtonData("✅ Все сервисы", "sync:all")), tgbotapi.NewInlineKeyboardRow(tgbotapi.NewInlineKeyboardButtonData("🎯 По сервисам", "sync:services")), tgbotapi.NewInlineKeyboardRow(tgbotapi.NewInlineKeyboardButtonData("🏠 Главное меню", "back:main")))
}
func confirmKB() tgbotapi.InlineKeyboardMarkup {
	return tgbotapi.NewInlineKeyboardMarkup(tgbotapi.NewInlineKeyboardRow(tgbotapi.NewInlineKeyboardButtonData("✅ Подтвердить", "start:selected"), tgbotapi.NewInlineKeyboardButtonData("❌ Отмена", "back:main")))
}
func (b *Bot) servicesKB(s *state) tgbotapi.InlineKeyboardMarkup {
	rows := [][]tgbotapi.InlineKeyboardButton{}
	for _, v := range b.cfg.Services {
		mark := "☐"
		if s.selected[v] {
			mark = "☑"
		}
		rows = append(rows, tgbotapi.NewInlineKeyboardRow(tgbotapi.NewInlineKeyboardButtonData(mark+" "+v, "toggle:"+v)))
	}
	rows = append(rows, tgbotapi.NewInlineKeyboardRow(tgbotapi.NewInlineKeyboardButtonData("✅ Выбрать все", "select:all"), tgbotapi.NewInlineKeyboardButtonData("▶️ Запустить", "start:selected")), tgbotapi.NewInlineKeyboardRow(tgbotapi.NewInlineKeyboardButtonData("🏠 Главное меню", "back:main")))
	return tgbotapi.InlineKeyboardMarkup{InlineKeyboard: rows}
}
