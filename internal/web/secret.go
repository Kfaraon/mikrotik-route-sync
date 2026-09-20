package web

import (
	"crypto/sha256"
	"encoding/hex"
	"log/slog"
	"strings"
	"sync/atomic"
	"time"
)

// ============================== Secret Field Registry ==============================

// secretFields — список ключей конфигурации, значения которых считаются секретными.
// Они маскируются в UI, API, CLI и никогда не логируются в открытом виде.
var secretFields = map[string]struct{}{
	"mikrotik.password":       {},
	"telegram.bot_token":      {},
	"web.auth.password":       {},
	"external.akamai_api_key": {},
	// _FILE-варианты не содержат секретов, но тоже маскируем для единообразия
	"mikrotik.password_file":       {},
	"telegram.bot_token_file":      {},
	"web.auth.password_file":       {},
	"external.akamai_api_key_file": {},
}

// IsSecret возвращает true, если ключ содержит секретное значение.
// Сравнение регистронезависимое, точки нормализованы.
func IsSecret(key string) bool {
	k := strings.ToLower(strings.TrimSpace(key))
	k = strings.ReplaceAll(k, "_", ".")
	_, ok := secretFields[k]
	return ok
}

// IsSecretPartial проверяет, содержит ли путь ключа секретный сегмент (например, "password").
// Используется для защиты от случайной утечки через новые поля конфига.
func IsSecretPartial(key string) bool {
	lower := strings.ToLower(key)
	suspicious := []string{"password", "token", "secret", "api_key", "apikey"}
	for _, s := range suspicious {
		if strings.Contains(lower, s) {
			return true
		}
	}
	return false
}

// ============================== Masking ==============================

// Mask возвращает маску для секретного значения.
// Правила:
//   - пустая строка -> ""
//   - длина <= 4    -> "••••"
//   - иначе         -> "••••••••" + последние 4 символа (чтобы пользователь мог опознать, какой токен установлен)
func Mask(value string) string {
	if value == "" {
		return ""
	}
	if len(value) <= 4 {
		return "••••"
	}
	return "••••••••" + value[len(value)-4:]
}

// MaskOrPlaceholder — вариант для полей форм, когда значение не установлено.
func MaskOrPlaceholder(value, placeholder string) string {
	if value == "" {
		return placeholder
	}
	return Mask(value)
}

// ============================== Fingerprinting (для аудита и сравнения) ==============================

// Fingerprint возвращает короткий хеш секрета (для логов и аудита, НЕ для хранения).
// Используется, когда нужно убедиться, что секрет изменился, не раскрывая его.
func Fingerprint(value string) string {
	if value == "" {
		return "<empty>"
	}
	sum := sha256.Sum256([]byte(value))
	return "sha256:" + hex.EncodeToString(sum[:])[:12]
}

// ============================== Audit ==============================

// SecretAudit — простой счётчик обращений к секретам и последняя маска.
// Помогает обнаруживать аномальные паттерны (массовые чтения = возможная утечка).
type SecretAudit struct {
	accessCount atomic.Int64
	lastAccess  atomic.Value // time.Time
	lastKey     atomic.Value // string
}

var audit = &SecretAudit{}

// RecordAccess регистрирует обращение к секретному ключу.
func RecordAccess(key, remoteIP string) {
	audit.accessCount.Add(1)
	audit.lastAccess.Store(time.Now())
	audit.lastKey.Store(key)

	slog.Info("secret accessed",
		"key", key,
		"fingerprint", Fingerprint(key),
		"remote", remoteIP,
		"total_accesses", audit.accessCount.Load(),
	)
}

// AuditStats возвращает текущую статистику обращений.
func AuditStats() map[string]any {
	var lastAccess time.Time
	if v := audit.lastAccess.Load(); v != nil {
		lastAccess = v.(time.Time)
	}
	lastKey, _ := audit.lastKey.Load().(string)
	return map[string]any{
		"total_accesses": audit.accessCount.Load(),
		"last_access":    lastAccess,
		"last_key":       lastKey,
	}
}

// ============================== Redaction для логов/ошибок ==============================

// RedactMap возвращает копию map[string]string, в которой секретные ключи заменены на маску.
// Используется при логировании структур и отправке в Telegram/API.
func RedactMap(m map[string]string) map[string]string {
	if m == nil {
		return nil
	}
	out := make(map[string]string, len(m))
	for k, v := range m {
		if IsSecret(k) || IsSecretPartial(k) {
			out[k] = Mask(v)
		} else {
			out[k] = v
		}
	}
	return out
}

// RedactAny пытается извлечь из any значения ключей и маскирует секретные.
// Возвращает исходный объект, если его структура не опознана.
func RedactAny(v any) any {
	if m, ok := v.(map[string]any); ok {
		out := make(map[string]any, len(m))
		for k, val := range m {
			if IsSecret(k) || IsSecretPartial(k) {
				if s, ok := val.(string); ok {
					out[k] = Mask(s)
				} else {
					out[k] = "••••••••"
				}
			} else {
				out[k] = RedactAny(val)
			}
		}
		return out
	}
	return v
}

// ============================== Validation ==============================

// ValidateSecretStrength — минимальная проверка стойкости пароля/токена.
// Правила:
//   - длина >= 8
//   - не равна "CHANGE_ME", "password", "123456"
//
// Возвращает ошибку с описанием, если пароль слабый.
func ValidateSecretStrength(key, value string) error {
	if value == "" {
		return nil // пустое — допустимо, проверка обязательности — на уровне конфига
	}
	if len(value) < 8 {
		return &WeakSecretError{Key: key, Reason: "too short (min 8)"}
	}
	weak := []string{"CHANGE_ME", "password", "123456", "admin", "qwerty", "secret"}
	lower := strings.ToLower(value)
	for _, w := range weak {
		if lower == w {
			return &WeakSecretError{Key: key, Reason: "weak default value"}
		}
	}
	return nil
}

// WeakSecretError — структурированная ошибка слабого секрета.
type WeakSecretError struct {
	Key    string
	Reason string
}

func (e *WeakSecretError) Error() string {
	return "weak secret for key " + e.Key + ": " + e.Reason
}
