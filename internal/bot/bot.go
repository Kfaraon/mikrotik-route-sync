package bot

import (
    "context"
    "fmt"
    "log/slog"
    "strconv"
    "strings"
    "sync"

    tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"

    "github.com/example/mikrotik-route-sync/internal/config"
    "github.com/example/mikrotik-route-sync/internal/core"
)

type state struct {
    screen   string // "main", "sync_menu", "sync_services", "confirm"
    selected map[string]bool
}

type bot struct {
    cfg       *config.Config
    syncer    *core.Syncer
    log       *slog.Logger
    api       *tgbotapi.BotAPI
    authorized map[int64]bool
    mtx       sync.Mutex
    states    map[int64]*state
}

func Run(ctx context.Context, cfg *config.Config, syncer *core.Syncer, log *slog.Logger) error {
    if !cfg.Telegram.Enabled || cfg.Telegram.BotToken == "" {
        return nil
    }
    api, err := tgbotapi.NewBotAPI(cfg.Telegram.BotToken)
    if err != nil {
        return err
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
            _, _ = b.api.Send(tgbotapi.NewMessage(chatID, "/menu /status /sync /schedule /help"))
        }
        return
    }

    if upd.CallbackQuery != nil {
        b.handleCallback(upd.CallbackQuery)
    }
}

func (b *bot) handleCallback(cb *tgbotapi.CallbackQuery) {
    chatID := cb.Message.Chat.ID
    if !b.authorized[chatID] {
        return
    }
    b.mtx.Lock()
    st := b.states[chatID]
    if st == nil {
        st = &state{screen: "main", selected: map[string]bool{}}
        b.states[chatID] = st
    }
    b.mtx.Unlock()

    data := cb.Data
    var resp tgbotapi.EditMessageTextConfig

    switch {
    case data == "main":
        b.mtx.Lock()
        st.screen = "main"
        b.mtx.Unlock()
        resp = b.renderMain(chatID, cb.Message.MessageID)

    case data == "sync":
        b.mtx.Lock()
        st.screen = "sync_menu"
        b.mtx.Unlock()
        resp = b.renderSyncMenu(chatID, cb.Message.MessageID)

    case data == "sync:all":
        b.mtx.Lock()
        st.screen = "confirm"
        st.selected = map[string]bool{}
        for _, s := range b.cfg.Services {
            st.selected[s] = true
        }
        b.mtx.Unlock()
        resp = b.renderConfirm(chatID, cb.Message.MessageID, st)

    case data == "sync:services":
        b.mtx.Lock()
        st.screen = "sync_services"
        st.selected = map[string]bool{}
        b.mtx.Unlock()
        resp = b.renderServices(chatID, cb.Message.MessageID, st)

    case strings.HasPrefix(data, "toggle:"):
        svc := strings.TrimPrefix(data, "toggle:")
        b.mtx.Lock()
        st.selected[svc] = !st.selected[svc]
        b.mtx.Unlock()
        resp = b.renderServices(chatID, cb.Message.MessageID, st)

    case data == "select:all":
        b.mtx.Lock()
        for _, s := range b.cfg.Services {
            st.selected[s] = true
        }
        b.mtx.Unlock()
        resp = b.renderServices(chatID, cb.Message.MessageID, st)

    case data == "start:selected":
        var svcs []string
        b.mtx.Lock()
        for s, on := range st.selected {
            if on {
                svcs = append(svcs, s)
            }
        }
        st.screen = "main"
        b.mtx.Unlock()
        if len(svcs) == 0 {
            resp = tgbotapi.NewEditMessageText(chatID, cb.Message.MessageID, "Не выбрано ни одного сервиса.")
        } else {
            resp = tgbotapi.NewEditMessageText(chatID, cb.Message.MessageID,
                fmt.Sprintf("▶️ Запущено для: %s", strings.Join(svcs, ", ")))
            go func() { _ = b.syncer.SyncMany(context.Background(), svcs, false) }()
        }
    }
    resp.ChatID = chatID
    _, _ = b.api.Send(resp)
    _, _ = b.api.Request(tgbotapi.NewCallback(cb.ID, ""))
}

func (b *bot) showMain(chatID int64) {
    msg := tgbotapi.NewMessage(chatID, "Главное меню")
    msg.ReplyMarkup = mainMenu()
    _, _ = b.api.Send(msg)
}

func (b *bot) renderMain(chatID int64, msgID int) tgbotapi.EditMessageTextConfig {
    c := tgbotapi.NewEditMessageText(chatID, msgID, "Главное меню")
    m := mainMenu()
    c.ReplyMarkup = &m
    return c
}

func (b *bot) showSyncMenu(chatID int64) {
    msg := tgbotapi.NewMessage(chatID, "Что синхронизировать?")
    msg.ReplyMarkup = syncMenu()
    _, _ = b.api.Send(msg)
}

func (b *bot) renderSyncMenu(chatID int64, msgID int) tgbotapi.EditMessageTextConfig {
    c := tgbotapi.NewEditMessageText(chatID, msgID, "Что синхронизировать?")
    m := syncMenu()
    c.ReplyMarkup = &m
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
                tgbotapi.NewInlineKeyboardButtonData("❌ Отмена", "main"),
            },
        },
    }
    return c
}

func (b *bot) showStatus(chatID int64) {
    var lines []string
    for _, svc := range b.cfg.Services {
        routes, err := b.syncer.ListRoutes(context.Background(), svc)
        if err != nil {
            lines = append(lines, fmt.Sprintf("• %s: err", svc))
            continue
        }
        lines = append(lines, fmt.Sprintf("• %s: %d routes", svc, len(routes)))
    }
    _, _ = b.api.Send(tgbotapi.NewMessage(chatID, strings.Join(lines, "\n")))
}

func (b *bot) showSchedules(chatID int64) {
    var lines []string
    for _, svc := range b.cfg.Services {
        lines = append(lines, fmt.Sprintf("• %s: %s", svc, b.cfg.ScheduleFor(svc)))
    }
    _, _ = b.api.Send(tgbotapi.NewMessage(chatID, strings.Join(lines, "\n")))
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