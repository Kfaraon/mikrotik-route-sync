package scheduler

import "testing"

func TestHumanSchedules(t *testing.T) {
	for _, s := range []string{"every 6h", "daily at 03:00", "weekly on sunday at 04:00", "0 */6 * * *", "manual", "disabled"} {
		if _, e := CronSpec(s); e != nil {
			t.Fatalf("%s: %v", s, e)
		}
	}
}
