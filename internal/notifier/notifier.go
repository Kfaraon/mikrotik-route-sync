package notifier

import (
	"context"
	"fmt"
	"log/slog"
	"regexp"
	"strings"
	"time"

	"github.com/Kfaraon/mikrotik-route-sync/internal/config"
	"github.com/Kfaraon/mikrotik-route-sync/internal/logging"
	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
)

// SyncResult — результат синхронизации одного сервиса.
type SyncResult struct {
	Service    string `json:"service"`
	Success    bool   `json:"success"`
	Added      int    `json:"added"`
	Removed    int    `json:"removed"`
	Unchanged  int    `json:"unchanged"`
	Updated    int    `json:"updated"` // re-enable выключенных записей
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
	loc    *time.Location // часовой пояс из config.timezone для меток времени
	log    *slog.Logger
}

// htmlEscaper — экранирование для ParseMode HTML (единственная допустимая
// разметка: <b>, <i>, <code>, <a>; всё остальное экранируется).
var htmlEscaper = strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;")

// tagRE — упрощённое снятие тегов для запасной отправки plain text.
var tagRE = regexp.MustCompile(`<(?:/)?[a-zA-Z][^<>]*>`)

func esc(s string) string {
	return htmlEscaper.Replace(s)
}

// htmlToText превращает сообщение в чистый текст (запасной вариант отправки,
// если Telegram отклонил разметку).
func htmlToText(s string) string {
	s = tagRE.ReplaceAllString(s, "")
	return strings.NewReplacer("&lt;", "<", "&gt;", ">", "&amp;", "&").Replace(s)
}

// LoadLocation часовой пояс из конфига; при ошибке — UTC.
func LoadLocation(tz string) *time.Location {
	if tz == "" {
		return time.UTC
	}
	if loc, err := time.LoadLocation(tz); err == nil {
		return loc
	}
	return time.UTC
}

// NewTelegram создаёт Telegram-нотификатор.
func NewTelegram(cfg config.TelegramConfig, timezone string, log *slog.Logger) (Notifier, error) {
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
	return &TelegramNotifier{
		api:    api,
		chatID: chatID,
		loc:    LoadLocation(timezone),
		log:    log,
	}, nil
}

// FromConfig создаёт нотификатор из конфигурации; при ошибке или
// отключённом Telegram возвращает NoopNotifier (работа продолжается).
func FromConfig(cfg config.TelegramConfig, timezone string, log *slog.Logger) Notifier {
	n, err := NewTelegram(cfg, timezone, log)
	if err != nil {
		if log != nil {
			log.Error("telegram notifier init failed, notifications disabled", "err", err)
		}
		return &NoopNotifier{}
	}
	return n
}

// stamp — метка времени уведомления в формате 03.10.2026 20:04:40
// в часовом поясе config.timezone.
func (n *TelegramNotifier) stamp() string {
	loc := n.loc
	if loc == nil {
		loc = time.Local
	}
	return time.Now().In(loc).Format("02.01.2006 15:04:05")
}

// FormatDur — длительность в читаемом виде: 14,8 с / 2 мин 5 с / 1 ч 5 мин
// (вместо Go-строки вида 1.234567891s).
func FormatDur(d time.Duration) string {
	if d < time.Minute {
		return strings.ReplaceAll(fmt.Sprintf("%.1f", d.Seconds()), ".", ",") + " с"
	}
	m := int(d.Minutes())
	sec := int(d.Seconds()) % 60
	if m < 60 {
		return fmt.Sprintf("%d мин %d с", m, sec)
	}
	return fmt.Sprintf("%d ч %d мин", m/60, m%60)
}

// ============================== Русификация ошибок ==============================
//
// В коде и логах тексты ошибок остаются английскими (конвенция проекта),
// русификация выполняется только на границе Telegram: сообщения, уходящие
// пользователю, должны быть полностью на русском языке.

// errorExact — точные тексты ошибок.
var errorExact = map[string]string{
	"all desired prefixes overlap existing MikroTik entries, aborting sync (fail-closed)": "все желаемые префиксы уже перекрыты существующими записями MikroTik — синхронизация прервана (защита fail-closed)",
	"all desired prefixes overlap existing MikroTik entries":                              "все желаемые префиксы уже перекрыты существующими записями MikroTik",
	"no valid prefixes after validation, aborting sync (fail-closed)":                     "после валидации не осталось корректных префиксов — синхронизация прервана (защита fail-closed)",
	"no valid prefixes after validation":                                                  "после валидации не осталось корректных префиксов",
	"context cancelled":                                                                   "операция отменена",
	"snapshots are disabled":                                                              "снапшоты отключены",
	"nothing to update (disabled is empty)":                                               "обновлять нечего (поле disabled пусто)",
}

// errorRules — шаблоны с подстановками (порядок важен).
var errorRules = []struct {
	re  *regexp.Regexp
	tpl string
}{
	{regexp.MustCompile(`^deletion of all (\d+) managed entries is blocked \(new set would be empty; use --force or remove-service\)$`),
		"удаление всех $1 управляемых записей заблокировано: новый набор пуст (нужен --force или remove-service)"},
	{regexp.MustCompile(`^delete ratio ([\d.]+) exceeds maximum ([\d.]+) \((\d+) of (\d+) entries; use --force to override\)$`),
		"доля удаляемых записей $1 превышает максимум $2 ($3 из $4; обход через --force)"},
	{regexp.MustCompile(`^delete count (\d+) exceeds require_confirmation_over (\d+) \(use --force to override\)$`),
		"удаление $1 записей превышает порог require_confirmation_over $2 (обход через --force)"},
	{regexp.MustCompile(`^service (\S+) not found$`), "сервис $1 не найден"},
	{regexp.MustCompile(`^service (\S+) already exists$`), "сервис $1 уже существует"},
	{regexp.MustCompile(`^no prefixes collected for service (\S+)$`),
		"источник не вернул префиксов для сервиса $1"},
}

// opRule — операции транзакции: «add 1.0.0.0/32: …» → «добавление 1.0.0.0/32: …».
var opRule = regexp.MustCompile(`^(add|update|delete) (\S+): `)

// errorPrefixes — обёртки ошибок: «английский префикс» → «русский префикс».
// Остаток локализуется рекурсивно (вложенное «transaction: add …: routeros …»).
var errorPrefixes = [][2]string{
	{"list entries: ", "получение записей списка: "},
	{"collect: ", "сбор адресов: "},
	{"validate: ", "валидация: "},
	{"compute diff: ", "расчёт изменений: "},
	{"transaction: ", "транзакция: "},
	{"panic: ", "внутренняя ошибка (panic): "},
	{"invalid service name: ", "недопустимое имя сервиса: "},
	{"save snapshot: ", "сохранение снапшота: "},
	{"load snapshot: ", "загрузка снапшота: "},
}

// errorInline — подстановки внутри текста (ошибки RouterOS и откат).
var errorInline = []struct {
	re  *regexp.Regexp
	tpl string
}{
	{regexp.MustCompile(`routeros (GET|PUT|POST|PATCH|DELETE) `), "MikroTik API $1 "},
	{regexp.MustCompile(`: status=`), ": статус="},
	{regexp.MustCompile(` body=`), " тело="},
	{regexp.MustCompile(`: decode: `), ": ошибка разбора ответа: "},
	{regexp.MustCompile(`; rollback failed: `), "; откат не выполнен: "},
	{regexp.MustCompile(`failure: already have such entry`), "запись уже существует"},
}

// opRu — перевод операций транзакции (правило ^(add|update|delete) …).
var opRu = map[string]string{"add": "добавление", "update": "обновление", "delete": "удаление"}

// LocalizeError переводит текст ошибки на русский язык для уведомлений.
// Неизвестные тексты возвращаются без изменений; русские тексты не ломаются.
func LocalizeError(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	out := localizeCore(s)
	for _, r := range errorInline {
		out = r.re.ReplaceAllString(out, r.tpl)
	}
	return out
}

func localizeCore(s string) string {
	if ru, ok := errorExact[s]; ok {
		return ru
	}
	for _, r := range errorRules {
		if r.re.MatchString(s) {
			return r.re.ReplaceAllString(s, r.tpl)
		}
	}
	if m := opRule.FindStringSubmatch(s); m != nil {
		return opRu[m[1]] + " " + m[2] + ": " + LocalizeError(s[len(m[0]):])
	}
	for _, p := range errorPrefixes {
		if rest, ok := strings.CutPrefix(s, p[0]); ok {
			return p[1] + LocalizeError(rest)
		}
	}
	return s
}

// Send отправляет сообщение (HTML). При отказе парсера разметки — повтор
// обычным текстом, чтобы уведомление дошло в любом случае.
func (n *TelegramNotifier) Send(ctx context.Context, message string) error {
	if n.chatID == 0 || n.api == nil {
		return nil
	}
	msg := tgbotapi.NewMessage(n.chatID, message)
	msg.ParseMode = tgbotapi.ModeHTML
	if _, err := n.api.Send(msg); err == nil {
		return nil
	}
	plain := tgbotapi.NewMessage(n.chatID, htmlToText(message))
	_, err := n.api.Send(plain)
	return err
}

// SyncStart — уведомление о старте синхронизации.
func (n *TelegramNotifier) SyncStart(ctx context.Context, services []string, trigger, spec string) error {
	var b strings.Builder
	b.WriteString("🚀 <b>Синхронизация запущена</b>\n")
	fmt.Fprintf(&b, "⏰ %s\n", n.stamp())

	list := strings.Join(services, ", ")
	if len(services) > 5 {
		list = strings.Join(services[:5], ", ") + fmt.Sprintf(" и ещё %d", len(services)-5)
	}
	fmt.Fprintf(&b, "📋 Сервисы (%d): %s\n", len(services), esc(list))
	if trigger != "" {
		fmt.Fprintf(&b, "🔍 Триггер: %s\n", esc(trigger))
	}
	if spec != "" {
		fmt.Fprintf(&b, "⚙️ Режим: %s", esc(spec))
	}
	return n.Send(ctx, b.String())
}

// SyncDone — итоговое уведомление: по каждому сервису и сводка.
func (n *TelegramNotifier) SyncDone(ctx context.Context, results []SyncResult, elapsed any, dryRun bool) error {
	var b strings.Builder
	if dryRun {
		b.WriteString("🔎 <b>Пробный запуск</b> — изменения не применялись\n")
	} else {
		b.WriteString("✅ <b>Синхронизация завершена</b>\n")
	}
	fmt.Fprintf(&b, "⏰ %s\n", n.stamp())

	okCount, failCount, totAdded, totRemoved := 0, 0, 0, 0
	for _, r := range results {
		if r.Success {
			okCount++
			totAdded += r.Added
			totRemoved += r.Removed
			fmt.Fprintf(&b, "\n✅ <b>%s</b> — ➕ %d · ➖ %d · 🔁 %d",
				esc(r.Service), r.Added, r.Removed, r.Unchanged)
			if r.Updated > 0 {
				fmt.Fprintf(&b, " · включено обратно %d", r.Updated)
			}
			b.WriteString("\n")
			continue
		}
		failCount++
		reason := "неизвестная ошибка"
		if strings.TrimSpace(r.Error) != "" {
			reason = LocalizeError(r.Error)
		}
		fmt.Fprintf(&b, "\n❌ <b>%s</b>\n   причина: %s\n",
			esc(r.Service), esc(reason))
	}

	if len(results) > 1 {
		fmt.Fprintf(&b, "\n📊 Итого: %d из %d успешно · ➕ %d · ➖ %d",
			okCount, len(results), totAdded, totRemoved)
		if failCount > 0 {
			fmt.Fprintf(&b, " · ❌ %d", failCount)
		}
	}
	if sp, ok := elapsed.(time.Duration); ok && sp > 0 {
		fmt.Fprintf(&b, "\n⏱ Время: %s", FormatDur(sp))
	}
	return n.Send(ctx, strings.TrimRight(b.String(), "\n"))
}

// Error — уведомление об ошибке синхронизации (секреты маскируются).
func (n *TelegramNotifier) Error(ctx context.Context, service string, err error) error {
	reason := "неизвестная ошибка"
	if err != nil {
		if loc := LocalizeError(logging.RedactError(err).Error()); loc != "" {
			reason = loc
		}
	}
	msg := "❌ <b>Ошибка синхронизации</b>\n" +
		"⏰ " + n.stamp() + "\n" +
		"🎯 Сервис: <b>" + esc(service) + "</b>\n" +
		"⚠️ " + esc(reason)
	return n.Send(ctx, msg)
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
