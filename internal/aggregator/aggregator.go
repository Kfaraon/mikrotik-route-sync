package aggregator

import (
	"fmt"
	"math/big"
	"net/netip"
	"sort"

	"github.com/Kfaraon/mikrotik-route-sync/pkg/cidrutil"
)

// Aggregate выполняет безопасную агрегацию CIDR-префиксов.
// Алгоритм:
// 1. Удаляет вложенные префиксы
// 2. Повторяет объединение sibling-префиксов (префиксы с одинаковой маской, отличающиеся только последним битом)
// 3. Проверяет инварианты:
//    - Сумма адресов до агрегации = сумма адресов после агрегации
//    - Каждый исходный префикс полностью покрыт результирующим набором
func Aggregate(in []netip.Prefix) ([]netip.Prefix, error) {
	if len(in) == 0 {
		return []netip.Prefix{}, nil
	}

	// Удаляем вложенные префиксы
	in = cidrutil.RemoveContained(in)
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
			// Проверяем, являются ли cur[i] и cur[i+1] siblings
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

		// Удаляем вложенные префиксы после объединения
		cur = cidrutil.RemoveContained(out)

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

// siblings проверяет, являются ли два префикса siblings (могут быть объединены в родительский префикс).
// Два префикса являются siblings, если:
// - У них одинаковая маска
// - Маска не равна 0 (нельзя объединить /0)
// - Они принадлежат одной адресной семье (IPv4 или IPv6)
// - Они отличаются только последним битом маски
// - Они не равны друг другу
func siblings(a, b netip.Prefix) (netip.Prefix, bool) {
	// Проверяем, что маски одинаковые и не равны 0
	if a.Bits() != b.Bits() || a.Bits() == 0 {
		return netip.Prefix{}, false
	}

	// Проверяем, что адресные семейства одинаковые (IPv4 и IPv6 имеют разную длину адреса)
	if a.Addr().BitLen() != b.Addr().BitLen() {
		return netip.Prefix{}, false
	}

	// Проверяем, что префиксы не равны
	if a == b {
		return netip.Prefix{}, false
	}

	// Вычисляем родительский префикс (маска на 1 меньше)
	parentBits := a.Bits() - 1
	pa := netip.PrefixFrom(a.Addr(), parentBits).Masked()
	pb := netip.PrefixFrom(b.Addr(), parentBits).Masked()

	// Если родительские префиксы совпадают, то a и b являются siblings
	if pa != pb {
		return netip.Prefix{}, false
	}

	return pa, true
}

// coverageInvariant проверяет, что каждый исходный префикс полностью покрыт результирующим набором.
func coverageInvariant(src, dst []netip.Prefix) bool {
	for _, s := range src {
		covered := false
		for _, d := range dst {
			if cidrutil.ContainsPrefix(d, s) {
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
// Использует big.Int для поддержки больших наборов адресов.
func countAddresses(prefixes []netip.Prefix) *big.Int {
	total := big.NewInt(0)

	for _, p := range prefixes {
		// Количество адресов в префиксе = 2^(address_bits - mask_bits)
		// Для IPv4: address_bits = 32, для IPv6: address_bits = 128
		addrBits := p.Addr().BitLen()
		hostBits := addrBits - p.Bits()

		if hostBits < 0 {
			// Некорректный префикс (маска больше длины адреса)
			continue
		}

		// Вычисляем 2^hostBits
		count := new(big.Int).Lsh(big.NewInt(1), uint(hostBits))
		total.Add(total, count)
	}

	return total
}
