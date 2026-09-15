package aggregator

import (
	"fmt"
	"math/big"
	"net/netip"
	"sort"
	"strings"
)

// ============================== CIDR Utilities ==============================

// RemoveContained удаляет префиксы, которые полностью содержатся в других префиксах.
// Например, если есть 192.168.0.0/16 и 192.168.1.0/24, то /24 будет удалён.
func RemoveContained(prefixes []netip.Prefix) []netip.Prefix {
	if len(prefixes) <= 1 {
		return prefixes
	}

	// Сортируем по размеру маски (широкие префиксы первыми)
	sorted := make([]netip.Prefix, len(prefixes))
	copy(sorted, prefixes)
	sort.Slice(sorted, func(i, j int) bool {
		if sorted[i].Bits() != sorted[j].Bits() {
			return sorted[i].Bits() < sorted[j].Bits()
		}
		return sorted[i].Addr().Compare(sorted[j].Addr()) < 0
	})

	// Удаляем дубликаты
	unique := make([]netip.Prefix, 0, len(sorted))
	seen := make(map[netip.Prefix]bool)
	for _, p := range sorted {
		masked := p.Masked()
		if !seen[masked] {
			seen[masked] = true
			unique = append(unique, masked)
		}
	}

	// Фильтруем вложенные префиксы
	result := make([]netip.Prefix, 0, len(unique))
	for i, p := range unique {
		contained := false
		for j := 0; j < i; j++ {
			if ContainsPrefix(unique[j], p) {
				contained = true
				break
			}
		}
		if !contained {
			result = append(result, p)
		}
	}

	return result
}

// ContainsPrefix проверяет, содержит ли префикс outer префикс inner.
func ContainsPrefix(outer, inner netip.Prefix) bool {
	outer = outer.Masked()
	inner = inner.Masked()

	if !outer.IsValid() || !inner.IsValid() {
		return false
	}

	if outer.Addr().Is4() != inner.Addr().Is4() {
		return false
	}

	if outer.Bits() > inner.Bits() {
		return false
	}

	if outer.Bits() == inner.Bits() {
		return outer == inner
	}

	maskedInner := netip.PrefixFrom(inner.Addr(), outer.Bits()).Masked()
	return maskedInner == outer
}

// NormalizeAll парсит список CIDR-строк и возвращает нормализованные префиксы.
func NormalizeAll(raw []string) ([]netip.Prefix, error) {
	result := make([]netip.Prefix, 0, len(raw))
	
	for _, s := range raw {
		s = strings.TrimSpace(s)
		if s == "" || strings.HasPrefix(s, "#") {
			continue
		}

		prefix, err := netip.ParsePrefix(s)
		if err != nil {
			addr, addrErr := netip.ParseAddr(s)
			if addrErr != nil {
				return nil, err
			}
			bits := 32
			if addr.Is6() {
				bits = 128
			}
			prefix = netip.PrefixFrom(addr, bits)
		}

		prefix = prefix.Masked()
		if !prefix.IsValid() {
			continue
		}

		result = append(result, prefix)
	}

	return result, nil
}

// ============================== Validation ==============================

// Минимальная длина маски: /9 и уже — ок, /8 и шире — отклоняем для IPv4
const minV4Bits = 9
const minV6Bits = 32

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

var reservedV6 = func() []netip.Prefix {
	raw := []string{
		"::/128", "::1/128", "::ffff:0:0/96", "64:ff9b::/96",
		"100::/64", "2001:db8::/32", "fc00::/7", "fe80::/10", "ff00::/8",
	}
	out := make([]netip.Prefix, 0, len(raw))
	for _, s := range raw {
		out = append(out, netip.MustParsePrefix(s))
	}
	return out
}()

// Validate проверяет префикс на валидность и отсутствие пересечений с зарезервированными диапазонами.
func Validate(p netip.Prefix) error {
	if !p.IsValid() {
		return fmt.Errorf("invalid prefix")
	}
	p = p.Masked()
	if p.Addr().Is4() {
		if p.Bits() < minV4Bits {
			return fmt.Errorf("prefix %s too wide (min /%d)", p, minV4Bits)
		}
		for _, r := range reservedV4 {
			if r.Overlaps(p) {
				return fmt.Errorf("prefix %s overlaps reserved %s", p, r)
			}
		}
		return nil
	}
	if p.Bits() < minV6Bits {
		return fmt.Errorf("prefix %s too wide (min /%d)", p, minV6Bits)
	}
	for _, r := range reservedV6 {
		if r.Overlaps(p) {
			return fmt.Errorf("prefix %s overlaps reserved %s", p, r)
		}
	}
	return nil
}

// FilterValid фильтрует список префиксов, возвращая только валидные.
func FilterValid(in []netip.Prefix) ([]netip.Prefix, []error) {
	out := make([]netip.Prefix, 0, len(in))
	var errs []error
	for _, p := range in {
		if err := Validate(p); err != nil {
			errs = append(errs, err)
			continue
		}
		out = append(out, p.Masked())
	}
	return out, errs
}

// ============================== Aggregation ==============================

// Aggregate выполняет безопасную агрегацию CIDR-префиксов.
// Алгоритм:
// 1. Удаляет вложенные префиксы
// 2. Повторяет объединение sibling-префиксов
// 3. Проверяет инварианты:
//    - Сумма адресов до агрегации = сумма адресов после агрегации
//    - Каждый исходный префикс полностью покрыт результирующим набором
func Aggregate(in []netip.Prefix) ([]netip.Prefix, error) {
	if len(in) == 0 {
		return []netip.Prefix{}, nil
	}

	// Удаляем вложенные префиксы
	in = RemoveContained(in)
	if len(in) == 0 {
		return []netip.Prefix{}, nil
	}

	// Считаем сумму адресов до агрегации
	srcCount := countAddresses(in)

	// Итеративно объединяем sibling-префиксы
	cur := append([]netip.Prefix(nil), in...)
	for {
		sort.Slice(cur, func(i, j int) bool {
			if cur[i].Addr().Compare(cur[j].Addr()) == 0 {
				return cur[i].Bits() < cur[j].Bits()
			}
			return cur[i].Addr().Compare(cur[j].Addr()) < 0
		})

		changed := false
		out := make([]netip.Prefix, 0, len(cur))

		for i := 0; i < len(cur); {
			if i+1 < len(cur) {
				if p, ok := siblings(cur[i], cur[i+1]); ok {
					out = append(out, p)
					i += 2
					changed = true
					continue
				}
			}
			out = append(out, cur[i])
			i++
		}

		cur = RemoveContained(out)

		if !changed {
			break
		}
	}

	// Проверяем инвариант суммы адресов
	dstCount := countAddresses(cur)
	if srcCount.Cmp(dstCount) != 0 {
		return nil, fmt.Errorf("aggregation address count invariant failed: src=%v, dst=%v", srcCount, dstCount)
	}

	// Проверяем инвариант покрытия
	if !coverageInvariant(in, cur) {
		return nil, fmt.Errorf("aggregation coverage invariant failed")
	}

	return cur, nil
}

// AggregateStrings — обёртка для работы со строками.
func AggregateStrings(prefixes []string) ([]string, error) {
	normalized, err := NormalizeAll(prefixes)
	if err != nil {
		return nil, err
	}

	aggregated, err := Aggregate(normalized)
	if err != nil {
		return nil, err
	}

	result := make([]string, len(aggregated))
	for i, p := range aggregated {
		result[i] = p.String()
	}
	return result, nil
}

// siblings проверяет, являются ли два префикса siblings.
func siblings(a, b netip.Prefix) (netip.Prefix, bool) {
	if a.Bits() != b.Bits() || a.Bits() == 0 {
		return netip.Prefix{}, false
	}

	if a.Addr().BitLen() != b.Addr().BitLen() {
		return netip.Prefix{}, false
	}

	if a == b {
		return netip.Prefix{}, false
	}

	parentBits := a.Bits() - 1
	pa := netip.PrefixFrom(a.Addr(), parentBits).Masked()
	pb := netip.PrefixFrom(b.Addr(), parentBits).Masked()

	if pa != pb {
		return netip.Prefix{}, false
	}

	return pa, true
}

// coverageInvariant проверяет, что каждый исходный префикс покрыт результирующим набором.
func coverageInvariant(src, dst []netip.Prefix) bool {
	for _, s := range src {
		covered := false
		for _, d := range dst {
			if ContainsPrefix(d, s) {
				covered = true
				break
			}
		}
		if !covered {
			return false
		}
	}
	return true
}

// countAddresses вычисляет общее количество IP-адресов в наборе префиксов.
func countAddresses(prefixes []netip.Prefix) *big.Int {
	total := big.NewInt(0)

	for _, p := range prefixes {
		addrBits := p.Addr().BitLen()
		hostBits := addrBits - p.Bits()

		if hostBits < 0 {
			continue
		}

		count := new(big.Int).Lsh(big.NewInt(1), uint(hostBits))
		total.Add(total, count)
	}

	return total
}
