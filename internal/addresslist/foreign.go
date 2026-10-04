package addresslist

import (
	"net/netip"

	"github.com/Kfaraon/mikrotik-route-sync/internal/aggregator"
)

// ForeignEntries — записи списка, НЕ принадлежащие сервису: ручные записи,
// записи других сервисов и dynamic. Приложение их никогда не изменяет
// (PROMPT II.1 — изоляция), но учитывает при формировании желаемого набора,
// чтобы не создавать пересекающиеся адреса.
func ForeignEntries(entries []Entry, list, comment string) []Entry {
	out := make([]Entry, 0, len(entries))
	for _, e := range entries {
		if e.List != list {
			continue
		}
		if e.Comment == comment && !e.IsDynamic() {
			continue // собственные управляемые записи сервиса
		}
		out = append(out, e)
	}
	return out
}

// SubtractForeign вычитает чужие записи из желаемого набора адресов сервиса.
//
// Если префикс сервиса пересекается (равенство, вложение в любую сторону) с уже
// существующей записью MikroTik, из него вычитается чужая сеть (Split, как в
// overrides.exclude) либо он исчезает целиком. Пересечения чужих записей между
// собой не учитываются — важен только результат для desired.
//
// Возвращает обновлённый набор и число желаемых префиксов, столкнувшихся с
// чужими записями (для логов). Некорректные и не-IPv4 адреса чужих записей
// пропускаются: охватить их невозможно.
func SubtractForeign(desired []string, foreign []Entry) ([]string, int) {
	if len(desired) == 0 || len(foreign) == 0 {
		return desired, 0
	}

	blocks := make([]netip.Prefix, 0, len(foreign))
	for _, e := range foreign {
		n, err := NormalizeAddress(e.Address)
		if err != nil {
			continue
		}
		p, err := netip.ParsePrefix(n)
		if err != nil {
			continue
		}
		blocks = append(blocks, p)
	}
	if len(blocks) == 0 {
		return desired, 0
	}

	out := make([]string, 0, len(desired))
	hits := 0
	for _, d := range desired {
		p, err := netip.ParsePrefix(d)
		if err != nil || !p.Addr().Is4() {
			out = append(out, d)
			continue
		}

		pieces := []netip.Prefix{p.Masked()}
		hit := false
		for _, b := range blocks {
			next := make([]netip.Prefix, 0, len(pieces))
			for _, piece := range pieces {
				if !prefixesOverlap(piece, b) {
					next = append(next, piece)
					continue
				}
				hit = true
				next = append(next, aggregator.SubtractPrefix(piece, b)...)
			}
			pieces = next
			if len(pieces) == 0 {
				break
			}
		}
		if hit {
			hits++
		}
		for _, piece := range pieces {
			out = append(out, piece.String())
		}
	}
	return out, hits
}

// prefixesOverlap — для CIDR-блоков пересечение равно вложению в любую сторону.
func prefixesOverlap(a, b netip.Prefix) bool {
	return a.Contains(b.Addr()) || b.Contains(a.Addr())
}
