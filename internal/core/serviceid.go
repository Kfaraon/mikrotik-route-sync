package core

// Идентичность сервиса: каждому добавлению выдаётся UUID v4, который
// отличает текущее воплощение сервиса от прежнего с тем же именем.
// Нужно, чтобы после удаления и повторного добавления сервиса история,
// логи и дашборд не показывали информацию старого воплощения.

import (
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"sync/atomic"
	"time"
)

// serviceIDFallbackCounter — монотонный счётчик для fallback UUID.
var serviceIDFallbackCounter atomic.Uint64

// newServiceID возвращает UUID v4 (RFC 4122) в каноническом виде
// (8-4-4-4-12). Уникальность важнее непредсказуемости: при отказе
// crypto/rand (на практике не случается на Linux) берём метку времени
// со счётчиком — коллизия исключена в пределах процесса.
func newServiceID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fallbackServiceID()
	}
	b[6] = (b[6] & 0x0f) | 0x40 // версия 4
	b[8] = (b[8] & 0x3f) | 0x80 // вариант RFC 4122
	return formatUUID(b)
}

func fallbackServiceID() string {
	var b [16]byte
	binary.BigEndian.PutUint64(b[0:8], uint64(time.Now().UnixNano()))
	binary.BigEndian.PutUint64(b[8:16], serviceIDFallbackCounter.Add(1))
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return formatUUID(b)
}

// formatUUID форматирует 16 байт в каноническую строку UUID.
func formatUUID(b [16]byte) string {
	var buf [36]byte
	hex.Encode(buf[0:8], b[0:4])
	buf[8] = '-'
	hex.Encode(buf[9:13], b[4:6])
	buf[13] = '-'
	hex.Encode(buf[14:18], b[6:8])
	buf[18] = '-'
	hex.Encode(buf[19:23], b[8:10])
	buf[23] = '-'
	hex.Encode(buf[24:36], b[10:16])
	return string(buf[:])
}
