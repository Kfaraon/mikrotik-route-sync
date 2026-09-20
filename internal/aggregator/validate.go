package aggregator

import (
	"fmt"
	"net/netip"
)

// reservedV4 — диапазоны, которые никогда не должны попадать в маршрутизацию
// (RFC 1918, loopback, link-local, CGNAT, TEST-NET, multicast, reserved).
// Проект работает только с IPv4 (PROMPT II.3, уровень 2).

var reservedV4 = func() []netip.Prefix {
	raw := []string{
		"0.0.0.0/8", "10.0.0.0/8", "100.64.0.0/10", "127.0.0.0/8",
		"169.254.0.0/16", "172.16.0.0/12", "192.0.0.0/24", "192.0.2.0/24",
		"192.88.99.0/24", "192.168.0.0/16", "198.18.0.0/15", "198.51.100.0/24",
		"203.0.113.0/24", "224.0.0.0/4", "240.0.0.0/4", "255.255.255.255/32",
	}
	out := make([]netip.Prefix, 0, len(raw))
	for _, s := range raw {
		out = append(out, netip.MustParsePrefix(s))
	}
	return out
}()

// ValidateAggregation проверяет инварианты безопасной агрегации:
//  1. сумма адресов до агрегации равна сумме после;
//  2. каждый исходный префикс полностью покрыт результирующим набором.
//
// Нарушение любого из инвариантов означает, что агрегация «раздула» или
// сузила покрытие, и её результат использовать нельзя.
func ValidateAggregation(before, after []netip.Prefix) error {
	if sumAddresses(before).Cmp(sumAddresses(after)) != 0 {
		return fmt.Errorf("aggregate invariant broken: address sum changed")
	}
	for _, in := range before {
		if !coversPrefix(after, in) {
			return fmt.Errorf("aggregate invariant broken: %s not covered by result", in)
		}
	}
	return nil
}

// coversPrefix проверяет, что префикс p целиком покрыт хотя бы одним
// префиксом из набора set.
func coversPrefix(set []netip.Prefix, p netip.Prefix) bool {
	p = p.Masked()
	for _, q := range set {
		q = q.Masked()
		if q.Addr().Is4() != p.Addr().Is4() {
			continue
		}
		if q.Bits() > p.Bits() {
			continue
		}
		// p ⊆ q тогда и только тогда, когда усечение p до маски q даёт ровно q.
		if netip.PrefixFrom(p.Addr(), q.Bits()).Masked() == q {
			return true
		}
	}
	return false
}

// SumAddresses — суммарное количество адресов (uint64, IPv6 клампится на MaxUint64).
func SumAddresses(ps []netip.Prefix) uint64 {
	var total uint64
	for _, p := range ps {
		bits := p.Bits()
		hostBits := p.Addr().BitLen() - bits
		if hostBits >= 64 {
			return ^uint64(0)
		}
		n := uint64(1) << hostBits
		if total > ^uint64(0)-n {
			return ^uint64(0)
		}
		total += n
	}
	return total
}
