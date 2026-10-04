package addresslist

import "fmt"

// SafetyParams — пороги safe-diff из config.Safety.
type SafetyParams struct {
	MaxDeleteRatio          float64
	RequireConfirmationOver int
}

// CheckDeletion проверяет защиту от массового удаления (PROMPT II.6)
// перед применением diff. existing — число управляемых записей сервиса
// сейчас, remove — сколько будет удалено.
//
//	removed / existing > max_delete_ratio      → ошибка (нужен --force)
//	removed > require_confirmation_over        → ошибка (нужен --force)
//	removed >= existing                        → ошибка всегда (новый набор пуст)
//
// existing == 0 проверок не требует.
func CheckDeletion(existing, remove int, p SafetyParams) error {
	if existing <= 0 || remove <= 0 {
		return nil
	}
	// PROMPT II.6: если existing > 0, а новый набор пуст — прерывать всегда,
	// без исключений, даже если ratio-пороги это допускают. Полное удаление
	// управляемых записей возможно только через remove-service или --force
	// (обход выполняется на уровне вызова, а не здесь).
	if remove >= existing {
		return fmt.Errorf(
			"deletion of all %d managed entries is blocked (new set would be empty; use --force or remove-service)",
			existing)
	}
	ratio := float64(remove) / float64(existing)
	if p.MaxDeleteRatio > 0 && ratio > p.MaxDeleteRatio {
		return fmt.Errorf(
			"delete ratio %.2f exceeds maximum %.2f (%d of %d entries; use --force to override)",
			ratio, p.MaxDeleteRatio, remove, existing)
	}
	if p.RequireConfirmationOver > 0 && remove > p.RequireConfirmationOver {
		return fmt.Errorf(
			"delete count %d exceeds require_confirmation_over %d (use --force to override)",
			remove, p.RequireConfirmationOver)
	}
	return nil
}
