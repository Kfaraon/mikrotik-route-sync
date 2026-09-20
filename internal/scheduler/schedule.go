package scheduler

import (
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/robfig/cron/v3"
)

var everyRE = regexp.MustCompile(`^every\s+(\d+(?:\.\d+)?(?:s|m|h))$`)
var dailyRE = regexp.MustCompile(`^daily at\s+([01]?\d|2[0-3]):([0-5]\d)$`)
var weeklyRE = regexp.MustCompile(`^weekly on\s+(sunday|monday|tuesday|wednesday|thursday|friday|saturday) at\s+([01]?\d|2[0-3]):([0-5]\d)$`)

func CronSpec(s string) (string, error) {
	s = strings.ToLower(strings.TrimSpace(s))
	if s == "manual" || s == "disabled" || s == "inherit" {
		return s, nil
	}
	if strings.HasPrefix(s, "@every ") || s == "@daily" || s == "@weekly" {
		return s, nil
	}
	if m := everyRE.FindStringSubmatch(s); m != nil {
		return "@every " + m[1], nil
	}
	if m := dailyRE.FindStringSubmatch(s); m != nil {
		return fmt.Sprintf("%s %s * * *", m[2], m[1]), nil
	}
	if m := weeklyRE.FindStringSubmatch(s); m != nil {
		days := map[string]int{"sunday": 0, "monday": 1, "tuesday": 2, "wednesday": 3, "thursday": 4, "friday": 5, "saturday": 6}
		return fmt.Sprintf("%s %s * * %d", m[3], m[2], days[m[1]]), nil
	}
	p := cron.NewParser(cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow | cron.Descriptor)
	if _, e := p.Parse(s); e != nil {
		return "", e
	}
	return s, nil
}
func Validate(s string) error { _, e := CronSpec(s); return e }
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
