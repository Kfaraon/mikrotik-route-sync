// Package addresslist — целевая модель управления: записи Firewall Address
// List RouterOS v7 (/ip/firewall/address-list).
//
// Один глобальный список (firewall.address_list, например TO-VPN); изоляция
// сервисов достигается комментарием AUTO:<service> (PROMPT I, II.1).
package addresslist

import (
	"fmt"
	"net/netip"
	"strings"
)

// Entry — запись /ip/firewall/address-list в формате RouterOS REST API.
type Entry struct {
	ID       string `json:".id,omitempty"`
	Address  string `json:"address"`
	List     string `json:"list"`
	Comment  string `json:"comment,omitempty"`
	Disabled string `json:"disabled,omitempty"` // "true"/"false"; "" == false
	Dynamic  string `json:"dynamic,omitempty"`  // "true" — динамическая запись
}

// CommentFor — AUTO-комментарий сервиса, генерируется автоматически.
func CommentFor(prefix, service string) string {
	return prefix + ":" + service
}

// IsDynamic — dynamic-записи не трогаем никогда (PROMPT I.Управляемая запись).
func (e Entry) IsDynamic() bool {
	return strings.EqualFold(e.Dynamic, "true")
}

// IsDisabled — трактует пустое значение как false (RouterOS не отдаёт поле).
func (e Entry) IsDisabled() bool {
	return strings.EqualFold(e.Disabled, "true")
}

// NormalizeAddress приводит address к каноническому виду (обнуление хостовых
// бит) либо пропускает одиночный IPv4 без маски как /32 (RouterOS допускает
// оба варианта). Сравниваем всегда по нормализованному значению (PROMPT II.7).
func NormalizeAddress(address string) (string, error) {
	address = strings.TrimSpace(address)
	if address == "" {
		return "", fmt.Errorf("empty address")
	}
	if !strings.Contains(address, "/") {
		addr, err := netip.ParseAddr(address)
		if err != nil {
			return "", fmt.Errorf("invalid address %q: %w", address, err)
		}
		if !addr.Is4() {
			return "", fmt.Errorf("IPv6 is not supported: %q", address)
		}
		return addr.String() + "/32", nil
	}
	p, err := netip.ParsePrefix(address)
	if err != nil {
		return "", fmt.Errorf("invalid prefix %q: %w", address, err)
	}
	if !p.Addr().Is4() {
		return "", fmt.Errorf("IPv6 is not supported: %q", address)
	}
	return p.Masked().String(), nil
}

// FilterManaged оставляет только управляемые записи сервиса:
//
//	list = <global list>
//	comment = AUTO:<service>
//	dynamic = false
//
// (PROMPT II.2 — автоматический поиск управляемых записей).
func FilterManaged(entries []Entry, list, comment string) []Entry {
	out := make([]Entry, 0, len(entries))
	for _, e := range entries {
		if e.List != list || e.Comment != comment || e.IsDynamic() {
			continue
		}
		out = append(out, e)
	}
	return out
}

// HasAutoPrefix проверяет, что комментарий принадлежит автоматизации
// (для аудита orphan-записей, PROMPT IV.6).
func HasAutoPrefix(comment, prefix string) bool {
	return strings.HasPrefix(comment, prefix+":") &&
		len(comment) > len(prefix)+1
}

// ServiceFromComment извлекает имя сервиса из AUTO:<service>.
func ServiceFromComment(comment, prefix string) (string, bool) {
	if !HasAutoPrefix(comment, prefix) {
		return "", false
	}
	return comment[len(prefix)+1:], true
}
