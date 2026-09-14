package aggregator

import (
	"fmt"
	"net/netip"
	"sort"
)

// Aggregate объединяет список CIDR-префиксов в минимальный набор.
// Использует сортировку + итеративное слияние смежных сетей.
// Гарантия: сумма адресов до == сумма адресов после.
func Aggregate(prefixes []string) ([]string, error) {
	if len(prefixes) == 0 {
		return nil, nil
	}

	var list []netip.Prefix
	seen := make(map[netip.Prefix]struct{}, len(prefixes))

	for _, s := range prefixes {
		p, err := netip.ParsePrefix(s)
		if err != nil {
			return nil, fmt.Errorf("bad prefix %q: %w", s, err)
		}
		p = p.Masked()
		if _, ok := seen[p]; !ok {
			seen[p] = struct{}{}
			list = append(list, p)
		}
	}

	if len(list) == 0 {
		return nil, nil
	}

	// Сортировка: сначала по адресу, затем по длине префикса
	sort.Slice(list, func(i, j int) bool {
		if c := list[i].Addr().Compare(list[j].Addr()); c != 0 {
			return c < 0
		}
		return list[i].Bits() < list[j].Bits()
	})

	// Удаление вложенных префиксов (если /24 внутри /16 — оставляем /16)
	filtered := make([]netip.Prefix, 0, len(list))
	for _, p := range list {
		if len(filtered) == 0 {
			filtered = append(filtered, p)
			continue
		}
		last := filtered[len(filtered)-1]
		if last.Contains(p.Addr()) && last.Bits() <= p.Bits() {
			continue // p вложен в last
		}
		filtered = append(filtered, p)
	}
	list = filtered

	// Итеративное слияние смежных сетей одинакового размера
	changed := true
	for changed {
		changed = false
		merged := make([]netip.Prefix, 0, len(list))
		i := 0
		for i < len(list) {
			if i+1 < len(list) && canMerge(list[i], list[i+1]) {
				parent, err := mergeSafe(list[i])
				if err != nil {
					merged = append(merged, list[i])
					i++
					continue
				}
				merged = append(merged, parent)
				i += 2
				changed = true
			} else {
				merged = append(merged, list[i])
				i++
			}
		}
		list = merged
	}

	out := make([]string, len(list))
	for i, p := range list {
		out[i] = p.String()
	}

	return out, nil
}

// canMerge проверяет, можно ли объединить два смежных префикса одинакового размера
func canMerge(a, b netip.Prefix) bool {
	if a.Bits() != b.Bits() || a.Bits() == 0 || a.Addr().Is4() != b.Addr().Is4() {
		return false
	}

	// Безопасная проверка смежности
	addrA := a.Addr()
	addrB := b.Addr()

	// Упорядочиваем адреса
	if addrB.Less(addrA) {
		addrA, addrB = addrB, addrA
	}

	// Проверяем что addrB — следующий адрес после блока addrA
	// Для префикса /N размер блока = 2^(bits-N)
	nextAddr := nextNetworkAddr(addrA, a.Bits())
	if nextAddr.IsZero() {
		return false
	}

	return nextAddr == addrB
}

// nextNetworkAddr вычисляет первый адрес следующего блока такого же размера
func nextNetworkAddr(addr netip.Prefix, bits int) netip.Addr {
	a := addr.Addr()
	prefixLen := addr.Bits()

	if a.Is4() {
		addrBytes := a.As4()
		hostBits := 32 - prefixLen
		blockSize := uint32(1) << uint(hostBits)

		// Текущий адрес сети
		base := uint32(addrBytes[0])<<24 | uint32(addrBytes[1])<<16 |
			uint32(addrBytes[2])<<8 | uint32(addrBytes[3])

		next := base + blockSize
		if next < base { // overflow
			return netip.Addr{}
		}

		return netip.AddrFrom4([4]byte{
			byte(next >> 24),
			byte(next >> 16),
			byte(next >> 8),
			byte(next),
		})
	}

	// IPv6 — упрощённо через As16
	addrBytes := a.As16()
	hostBits := 128 - prefixLen
	if hostBits > 63 {
		return netip.Addr{} // слишком большой блок
	}

	// Работаем с последними 8 байтами для простоты
	low := uint64(addrBytes[8])<<56 | uint64(addrBytes[9])<<48 |
		uint64(addrBytes[10])<<40 | uint64(addrBytes[11])<<32 |
		uint64(addrBytes[12])<<24 | uint64(addrBytes[13])<<16 |
		uint64(addrBytes[14])<<8 | uint64(addrBytes[15])

	blockSize := uint64(1) << uint(hostBits)
	next := low + blockSize
	if next < low { // overflow
		return netip.Addr{}
	}

	var result [16]byte
	copy(result[:8], addrBytes[:8])
	result[8] = byte(next >> 56)
	result[9] = byte(next >> 48)
	result[10] = byte(next >> 40)
	result[11] = byte(next >> 32)
	result[12] = byte(next >> 24)
	result[13] = byte(next >> 16)
	result[14] = byte(next >> 8)
	result[15] = byte(next)

	return netip.AddrFrom16(result)
}

// mergeSafe безопасно объединяет префикс в родительский
func mergeSafe(p netip.Prefix) (netip.Prefix, error) {
	if p.Bits() == 0 {
		return netip.Prefix{}, fmt.Errorf("cannot merge /0 prefix")
	}

	parentBits := p.Bits() - 1

	// Маскируем адрес под родительский префикс
	parent := netip.PrefixFrom(p.Addr(), parentBits)
	parent = parent.Masked()

	return parent, nil
}
