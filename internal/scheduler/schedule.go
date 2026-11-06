package scheduler

import (
	"time"

	"github.com/Kfaraon/mikrotik-list-sync/internal/schedule"
)

// Тонкие обёртки: единый парсер живёт в internal/schedule (его использует
// также config.Validate). Публичный API планировщика не меняется.
func CronSpec(s string) (string, error) { return schedule.CronSpec(s) }
func Validate(s string) error           { return schedule.Validate(s) }
func Next(s string, loc *time.Location, from time.Time) (time.Time, error) {
	return schedule.Next(s, loc, from)
}
