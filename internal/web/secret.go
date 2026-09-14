package web

// Mask заменяет значение секрета на фиксированную строку.
func Mask(s string) string {
	if s == "" {
		return ""
	}
	return "••••••••"
}
