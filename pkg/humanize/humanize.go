package humanize

import (
	"fmt"
	"time"
)

// Duration форматирует длительность в человекочитаемый вид.
func Duration(d time.Duration) string {
	if d < time.Second {
		return fmt.Sprintf("%dms", d.Milliseconds())
	}
	
	if d < time.Minute {
		seconds := int(d.Seconds())
		return fmt.Sprintf("%ds", seconds)
	}
	
	if d < time.Hour {
		minutes := int(d.Minutes())
		seconds := int(d.Seconds()) % 60
		if seconds == 0 {
			return fmt.Sprintf("%dm", minutes)
		}
		return fmt.Sprintf("%dm %ds", minutes, seconds)
	}
	
	if d < 24*time.Hour {
		hours := int(d.Hours())
		minutes := int(d.Minutes()) % 60
		if minutes == 0 {
			return fmt.Sprintf("%dh", hours)
		}
		return fmt.Sprintf("%dh %dm", hours, minutes)
	}
	
	days := int(d.Hours()) / 24
	hours := int(d.Hours()) % 24
	if hours == 0 {
		return fmt.Sprintf("%dd", days)
	}
	return fmt.Sprintf("%dd %dh", days, hours)
}

// Bytes форматирует размер в байтах в человекочитаемый вид.
func Bytes(b int64) string {
	const unit = 1024
	if b < unit {
		return fmt.Sprintf("%d B", b)
	}
	
	div, exp := int64(unit), 0
	for n := b / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	
	return fmt.Sprintf("%.1f %ciB", float64(b)/float64(div), "KMGTPE"[exp])
}

// BytesDecimal форматирует размер в байтах в десятичном виде (SI).
func BytesDecimal(b int64) string {
	const unit = 1000
	if b < unit {
		return fmt.Sprintf("%d B", b)
	}
	
	div, exp := int64(unit), 0
	for n := b / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	
	return fmt.Sprintf("%.1f %cB", float64(b)/float64(div), "KMGTPE"[exp])
}

// Number форматирует число с разделителями тысяч.
func Number(n int64) string {
	if n < 1000 {
		return fmt.Sprintf("%d", n)
	}
	
	str := fmt.Sprintf("%d", n)
	result := make([]byte, 0, len(str)+(len(str)/3))
	
	for i, ch := range str {
		if i > 0 && (len(str)-i)%3 == 0 {
			result = append(result, ' ')
		}
		result = append(result, byte(ch))
	}
	
	return string(result)
}

// Percentage форматирует процентное соотношение.
func Percentage(value, total int) string {
	if total == 0 {
		return "0%"
	}
	percent := float64(value) / float64(total) * 100
	return fmt.Sprintf("%.1f%%", percent)
}

// TimeAgo форматирует время относительно текущего момента.
func TimeAgo(t time.Time) string {
	d := time.Since(t)
	
	if d < time.Minute {
		return "just now"
	}
	if d < time.Hour {
		minutes := int(d.Minutes())
		if minutes == 1 {
			return "1 minute ago"
		}
		return fmt.Sprintf("%d minutes ago", minutes)
	}
	if d < 24*time.Hour {
		hours := int(d.Hours())
		if hours == 1 {
			return "1 hour ago"
		}
		return fmt.Sprintf("%d hours ago", hours)
	}
	if d < 7*24*time.Hour {
		days := int(d.Hours()) / 24
		if days == 1 {
			return "1 day ago"
		}
		return fmt.Sprintf("%d days ago", days)
	}
	if d < 30*24*time.Hour {
		weeks := int(d.Hours()) / (7 * 24)
		if weeks == 1 {
			return "1 week ago"
		}
		return fmt.Sprintf("%d weeks ago", weeks)
	}
	if d < 365*24*time.Hour {
		months := int(d.Hours()) / (30 * 24)
		if months == 1 {
			return "1 month ago"
		}
		return fmt.Sprintf("%d months ago", months)
	}
	
	years := int(d.Hours()) / (365 * 24)
	if years == 1 {
		return "1 year ago"
	}
	return fmt.Sprintf("%d years ago", years)
}

// TimeUntil форматирует время до будущего момента.
func TimeUntil(t time.Time) string {
	d := time.Until(t)
	
	if d < 0 {
		return TimeAgo(t)
	}
	
	if d < time.Minute {
		return "in a moment"
	}
	if d < time.Hour {
		minutes := int(d.Minutes())
		if minutes == 1 {
			return "in 1 minute"
		}
		return fmt.Sprintf("in %d minutes", minutes)
	}
	if d < 24*time.Hour {
		hours := int(d.Hours())
		if hours == 1 {
			return "in 1 hour"
		}
		return fmt.Sprintf("in %d hours", hours)
	}
	if d < 7*24*time.Hour {
		days := int(d.Hours()) / 24
		if days == 1 {
			return "in 1 day"
		}
		return fmt.Sprintf("in %d days", days)
	}
	
	weeks := int(d.Hours()) / (7 * 24)
	if weeks == 1 {
		return "in 1 week"
	}
	return fmt.Sprintf("in %d weeks", weeks)
}

// Plural возвращает правильную форму слова во множественном числе.
func Plural(count int, singular, plural string) string {
	if count == 1 {
		return singular
	}
	return plural
}

// CountWithWord форматирует количество с правильным словом.
func CountWithWord(count int, singular, plural string) string {
	return fmt.Sprintf("%d %s", count, Plural(count, singular, plural))
}
