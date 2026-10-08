// Package presets — расписание зеркала.
//
// update_schedule говорит на том же языке, что расписания сервисов
// (cron, "every 6h", "daily at 03:00", "manual"/пусто — выкл).
// Здесь только трансляция в cron-выражение для robfig/cron.
package presets

import (
	"fmt"
	"strconv"
	"strings"
)

// CronExpr переводит spec расписания в cron-выражение.
// Пусто/manual/disabled → "" (выключено, без ошибки).
func CronExpr(spec string) (string, error) {
	spec = strings.TrimSpace(spec)
	if spec == "" {
		return "", nil
	}
	low := strings.ToLower(spec)
	if low == "manual" || low == "disabled" || low == "inherit" {
		return "", nil
	}
	if strings.Contains(spec, " ") && !strings.HasPrefix(low, "every") && !strings.HasPrefix(low, "daily") {
		// Похоже на готовый cron (5-6 полей) — отдаём как есть.
		if n := len(strings.Fields(spec)); n == 5 || n == 6 {
			return spec, nil
		}
		return "", fmt.Errorf("bad cron %q: want 5-6 fields", spec)
	}
	if rest, ok := strings.CutPrefix(low, "every "); ok {
		return everyExpr(strings.TrimSpace(rest))
	}
	if rest, ok := strings.CutPrefix(low, "daily at "); ok {
		hh, mm, err := parseHM(strings.TrimSpace(rest))
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("%d %d * * *", mm, hh), nil
	}
	return "", fmt.Errorf("bad schedule %q: want cron, 'every Nh|Nm', 'daily at HH:MM', manual", spec)
}

func everyExpr(rest string) (string, error) {
	if strings.HasSuffix(rest, "h") {
		n, err := strconv.Atoi(strings.TrimSuffix(rest, "h"))
		if err != nil || n < 1 || n > 23 {
			return "", fmt.Errorf("bad every %q: hours 1..23", rest)
		}
		return fmt.Sprintf("0 */%d * * *", n), nil
	}
	if strings.HasSuffix(rest, "m") {
		n, err := strconv.Atoi(strings.TrimSuffix(rest, "m"))
		if err != nil || n < 5 || n >= 60 {
			return "", fmt.Errorf("bad every %q: minutes 5..59", rest)
		}
		return fmt.Sprintf("*/%d * * * *", n), nil
	}
	return "", fmt.Errorf("bad every %q: want Nh or Nm", rest)
}

func parseHM(s string) (hh, mm int, err error) {
	parts := strings.SplitN(s, ":", 2)
	if len(parts) != 2 {
		return 0, 0, fmt.Errorf("bad time %q: want HH:MM", s)
	}
	hh, err = strconv.Atoi(strings.TrimSpace(parts[0]))
	if err != nil || hh < 0 || hh > 23 {
		return 0, 0, fmt.Errorf("bad hour in %q", s)
	}
	mm, err = strconv.Atoi(strings.TrimSpace(parts[1]))
	if err != nil || mm < 0 || mm > 59 {
		return 0, 0, fmt.Errorf("bad minute in %q", s)
	}
	return hh, mm, nil
}
