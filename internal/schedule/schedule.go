// Package schedule — единый разбор спецификаций расписания.
//
// Пакет намеренно не зависит от config/scheduler: его используют и
// планировщик (для запуска), и конфиг (для fail-fast валидации при загрузке).
// Раньше парсер жил только в scheduler, поэтому config.Load принимал
// "sometimes" в schedules.global: сервис молча никогда не синхронизировался,
// а ошибка появлялась лишь в логе планировщика.
package schedule

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/robfig/cron/v3"
)

var everyRE = regexp.MustCompile(`^every\s+(\d+(?:\.\d+)?(?:s|m|h))$`)
var dailyRE = regexp.MustCompile(`^daily at\s+([01]?\d|2[0-3]):([0-5]\d)$`)
var weeklyRE = regexp.MustCompile(`^weekly on\s+(sunday|monday|tuesday|wednesday|thursday|friday|saturday) at\s+([01]?\d|2[0-3]):([0-5]\d)$`)

// cronDescriptors — стандартные @-описатели robfig/cron. CronSpec приводит их
// к канонической 5-польной форме: планировщик понимает только её (иначе
// @daily/@weekly проходили валидацию, но молча не планировались).
var cronDescriptors = map[string]string{
	"@yearly":   "0 0 1 1 *",
	"@annually": "0 0 1 1 *",
	"@monthly":  "0 0 1 * *",
	"@weekly":   "0 0 * * 0",
	"@daily":    "0 0 * * *",
	"@midnight": "0 0 * * *",
	"@hourly":   "0 * * * *",
}

// CronSpec нормализует спецификацию расписания.
// Возвращает manual/disabled/inherit как есть, @every-форму для интервалов
// и 5-польный cron для всего остального.
func CronSpec(s string) (string, error) {
	s = strings.ToLower(strings.TrimSpace(s))
	if s == "manual" || s == "disabled" || s == "inherit" {
		return s, nil
	}
	if strings.HasPrefix(s, "@every ") {
		d := strings.TrimSpace(strings.TrimPrefix(s, "@every "))
		dur, err := time.ParseDuration(d)
		if err != nil || dur <= 0 {
			return "", fmt.Errorf("bad @every %q: positive duration required", d)
		}
		return "@every " + dur.String(), nil
	}
	if c, ok := cronDescriptors[s]; ok {
		return c, nil
	}
	if m := everyRE.FindStringSubmatch(s); m != nil {
		dur, err := time.ParseDuration(m[1])
		if err != nil || dur <= 0 {
			return "", fmt.Errorf("bad interval %q: positive duration required", m[1])
		}
		return "@every " + dur.String(), nil
	}
	if m := dailyRE.FindStringSubmatch(s); m != nil {
		h, _ := strconv.Atoi(m[1])
		min, _ := strconv.Atoi(m[2])
		return fmt.Sprintf("%d %d * * *", min, h), nil
	}
	if m := weeklyRE.FindStringSubmatch(s); m != nil {
		days := map[string]int{"sunday": 0, "monday": 1, "tuesday": 2, "wednesday": 3, "thursday": 4, "friday": 5, "saturday": 6}
		h, _ := strconv.Atoi(m[2])
		min, _ := strconv.Atoi(m[3])
		return fmt.Sprintf("%d %d * * %d", min, h, days[m[1]]), nil
	}
	p := cron.NewParser(cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow | cron.Descriptor)
	if _, e := p.Parse(s); e != nil {
		return "", e
	}
	return s, nil
}

// Validate проверяет спецификацию.
func Validate(s string) error { _, e := CronSpec(s); return e }

// Next возвращает следующий запуск в указанной зоне.
// manual/disabled/inherit дают нулевое время без ошибки.
func Next(s string, loc *time.Location, from time.Time) (time.Time, error) {
	spec, e := CronSpec(s)
	if e != nil {
		return time.Time{}, e
	}
	if spec == "manual" || spec == "disabled" || spec == "inherit" {
		return time.Time{}, nil
	}
	p := cron.NewParser(cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow | cron.Descriptor)
	sch, e := p.Parse(spec)
	if e != nil {
		return time.Time{}, e
	}
	return sch.Next(from.In(loc)), nil
}
