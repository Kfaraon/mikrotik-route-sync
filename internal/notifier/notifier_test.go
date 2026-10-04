package notifier

import (
	"testing"
	"time"
)

func TestFormatDur(t *testing.T) {
	cases := []struct {
		d    time.Duration
		want string
	}{
		{0, "0,0 с"},
		{800 * time.Millisecond, "0,8 с"},
		{14800 * time.Millisecond, "14,8 с"},
		{125 * time.Second, "2 мин 5 с"},
		{59*time.Minute + 59*time.Second, "59 мин 59 с"},
		{2*time.Hour + 5*time.Minute, "2 ч 5 мин"},
	}
	for _, c := range cases {
		if got := FormatDur(c.d); got != c.want {
			t.Errorf("FormatDur(%v) = %q, want %q", c.d, got, c.want)
		}
	}
}

func TestLoadLocation(t *testing.T) {
	if loc := LoadLocation("Asia/Yekaterinburg"); loc == nil || loc.String() != "Asia/Yekaterinburg" {
		t.Fatalf("LoadLocation valid = %v", loc)
	}
	if loc := LoadLocation("Bad/Zone"); loc == nil || loc != time.UTC {
		t.Fatalf("LoadLocation invalid = %v, want UTC", loc)
	}
	if loc := LoadLocation(""); loc != time.UTC {
		t.Fatalf("LoadLocation empty = %v, want UTC", loc)
	}
}

func TestLocalizeError(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"", ""},
		// Точные тексты fail-closed.
		{
			"all desired prefixes overlap existing MikroTik entries, aborting sync (fail-closed)",
			"все желаемые префиксы уже перекрыты существующими записями MikroTik — синхронизация прервана (защита fail-closed)",
		},
		{
			"no valid prefixes after validation, aborting sync (fail-closed)",
			"после валидации не осталось корректных префиксов — синхронизация прервана (защита fail-closed)",
		},
		{"context cancelled", "операция отменена"},
		// Вложенные обёртки: transaction → add → routeros.
		{
			"transaction: add 1.0.0.0/32: routeros PUT /ip/firewall/address-list: " +
				`status=400 body={"detail":"failure: already have such entry"}`,
			"транзакция: добавление 1.0.0.0/32: MikroTik API PUT /ip/firewall/address-list: " +
				`статус=400 тело={"detail":"запись уже существует"}`,
		},
		{"collect: no prefixes collected for service rutor.org",
			"сбор адресов: источник не вернул префиксов для сервиса rutor.org"},
		{"list entries: connection refused", "получение записей списка: connection refused"},
		{"panic: goroutine exploded", "внутренняя ошибка (panic): goroutine exploded"},
		// Безопасность (safe-diff).
		{
			"delete count 90 exceeds require_confirmation_over 80 (use --force to override)",
			"удаление 90 записей превышает порог require_confirmation_over 80 (обход через --force)",
		},
		{
			"delete ratio 1.00 exceeds maximum 0.50 (86 of 86 entries; use --force to override)",
			"доля удаляемых записей 1.00 превышает максимум 0.50 (86 из 86; обход через --force)",
		},
		{
			"deletion of all 86 managed entries is blocked (new set would be empty; use --force or remove-service)",
			"удаление всех 86 управляемых записей заблокировано: новый набор пуст (нужен --force или remove-service)",
		},
		// Русские и неизвестные тексты не меняются.
		{
			"Синхронизация rutor.org пропущена: предыдущий запуск ещё выполняется",
			"Синхронизация rutor.org пропущена: предыдущий запуск ещё выполняется",
		},
		{"some unknown failure", "some unknown failure"},
	}
	for _, c := range cases {
		if got := LocalizeError(c.in); got != c.want {
			t.Errorf("LocalizeError(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}
