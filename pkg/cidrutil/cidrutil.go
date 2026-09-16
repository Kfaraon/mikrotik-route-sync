package cidrutil

import (
	"fmt"
	"net/netip"
	"sort"
)

// Normalize нормализует CIDR, убирая хостовые биты.
func Normalize(prefix netip.Prefix) netip.Prefix {
	return prefix.Masked()
}

// Contains проверяет, содержит ли outerPrefix внутренний innerPrefix.
func Contains(outer, inner netip.Prefix) bool {
	return outer.Contains(inner.Addr()) && outer.Bits() <= inner.Bits()
}

// Overlaps проверяет, пересекаются ли два префикса.
func Overlaps(a, b netip.Prefix) bool {
	return a.Contains(b.Addr()) || b.Contains(a.Addr())
}

// Deduplicate удаляет точные дубликаты префиксов.
func Deduplicate(prefixes []netip.Prefix) []netip.Prefix {
	seen := make(map[netip.Prefix]bool)
	result := make([]netip.Prefix, 0, len(prefixes))
	
	for _, p := range prefixes {
		normalized := Normalize(p)
		if !seen[normalized] {
			seen[normalized] = true
			result = append(result, normalized)
		}
	}
	
	return result
}

// RemoveNested удаляет вложенные префиксы.
// Если /24 уже включён в /16, то /24 удаляется.
func RemoveNested(prefixes []netip.Prefix) []netip.Prefix {
	if len(prefixes) == 0 {
		return prefixes
	}
	
	// Сортируем по длине префикса (от более широких к более узким)
	sort.Slice(prefixes, func(i, j int) bool {
		if prefixes[i].Bits() != prefixes[j].Bits() {
			return prefixes[i].Bits() < prefixes[j].Bits()
		}
		return prefixes[i].Addr().Less(prefixes[j].Addr())
	})
	
	result := make([]netip.Prefix, 0, len(prefixes))
	
	for _, current := range prefixes {
		nested := false
		for _, existing := range result {
			if Contains(existing, current) {
				nested = true
				break
			}
		}
		if !nested {
			result = append(result, current)
		}
	}
	
	return result
}

// SortByAddress сортирует префиксы по адресу.
func SortByAddress(prefixes []netip.Prefix) {
	sort.Slice(prefixes, func(i, j int) bool {
		return prefixes[i].Addr().Less(prefixes[j].Addr())
	})
}

// SortByPrefixLength сортирует префиксы по длине префикса.
func SortByPrefixLength(prefixes []netip.Prefix) {
	sort.Slice(prefixes, func(i, j int) bool {
		if prefixes[i].Bits() != prefixes[j].Bits() {
			return prefixes[i].Bits() < prefixes[j].Bits()
		}
		return prefixes[i].Addr().Less(prefixes[j].Addr())
	})
}

// FilterPrivate удаляет приватные и служебные диапазоны.
func FilterPrivate(prefixes []netip.Prefix) []netip.Prefix {
	result := make([]netip.Prefix, 0, len(prefixes))
	
	for _, p := range prefixes {
		if !IsPrivateOrReserved(p) {
			result = append(result, p)
		}
	}
	
	return result
}

// IsPrivateOrReserved проверяет, является ли префикс приватным или служебным.
func IsPrivateOrReserved(prefix netip.Prefix) bool {
	addr := prefix.Addr()
	
	if prefix.Addr().Is4() {
		return isPrivateIPv4(prefix)
	}
	return isPrivateIPv6(prefix)
}

func isPrivateIPv4(prefix netip.Prefix) bool {
	// RFC 1918 private ranges
	privateRanges := []netip.Prefix{
		netip.MustParsePrefix("10.0.0.0/8"),
		netip.MustParsePrefix("172.16.0.0/12"),
		netip.MustParsePrefix("192.168.0.0/16"),
		// CGNAT
		netip.MustParsePrefix("100.64.0.0/10"),
		// Link-local
		netip.MustParsePrefix("169.254.0.0/16"),
		// Loopback
		netip.MustParsePrefix("127.0.0.0/8"),
		// TEST-NET
		netip.MustParsePrefix("192.0.2.0/24"),
		netip.MustParsePrefix("198.51.100.0/24"),
		netip.MustParsePrefix("203.0.113.0/24"),
		// Benchmarking
		netip.MustParsePrefix("198.18.0.0/15"),
		// Multicast
		netip.MustParsePrefix("224.0.0.0/4"),
		// Reserved
		netip.MustParsePrefix("240.0.0.0/4"),
		// Broadcast
		netip.MustParsePrefix("255.255.255.255/32"),
		// IETF Protocol Assignments
		netip.MustParsePrefix("192.0.0.0/24"),
		// 6to4 Relay Anycast
		netip.MustParsePrefix("192.88.99.0/24"),
		// Current network
		netip.MustParsePrefix("0.0.0.0/8"),
	}
	
	for _, private := range privateRanges {
		if Contains(private, prefix) || prefix == private {
			return true
		}
	}
	
	return false
}

func isPrivateIPv6(prefix netip.Prefix) bool {
	privateRanges := []netip.Prefix{
		// Loopback
		netip.MustParsePrefix("::1/128"),
		// Unspecified
		netip.MustParsePrefix("::/128"),
		// IPv4-mapped
		netip.MustParsePrefix("::ffff:0:0/96"),
		// NAT64
		netip.MustParsePrefix("64:ff9b::/96"),
		// Discard-only
		netip.MustParsePrefix("100::/64"),
		// Documentation
		netip.MustParsePrefix("2001:db8::/32"),
		// Unique local
		netip.MustParsePrefix("fc00::/7"),
		// Link-local
		netip.MustParsePrefix("fe80::/10"),
		// Multicast
		netip.MustParsePrefix("ff00::/8"),
	}
	
	for _, private := range privateRanges {
		if Contains(private, prefix) || prefix == private {
			return true
		}
	}
	
	return false
}

// ValidatePrefix проверяет корректность префикса.
func ValidatePrefix(prefix netip.Prefix, minBitsV4, minBitsV6 int, allowHostRoutes bool) error {
	if !prefix.IsValid() {
		return fmt.Errorf("invalid prefix")
	}
	
	if prefix.Addr().Is4() {
		if prefix.Bits() < minBitsV4 {
			return fmt.Errorf("prefix /%d is too wide (minimum /%d)", prefix.Bits(), minBitsV4)
		}
		if !allowHostRoutes && prefix.Bits() == 32 {
			return fmt.Errorf("host routes (/32) are not allowed")
		}
	} else {
		if prefix.Bits() < minBitsV6 {
			return fmt.Errorf("prefix /%d is too wide (minimum /%d)", prefix.Bits(), minBitsV6)
		}
		if !allowHostRoutes && prefix.Bits() == 128 {
			return fmt.Errorf("host routes (/128) are not allowed")
		}
	}
	
	return nil
}

// CountAddresses подсчитывает количество адресов в префиксе.
func CountAddresses(prefix netip.Prefix) uint64 {
	bits := prefix.Bits()
	if prefix.Addr().Is4() {
		return 1 << (32 - bits)
	}
	// Для IPv6 возвращаем максимум uint64 для больших префиксов
	if bits <= 64 {
		return ^uint64(0) // max uint64
	}
	return 1 << (128 - bits)
}

// SumAddresses подсчитывает общее количество адресов в списке префиксов.
func SumAddresses(prefixes []netip.Prefix) uint64 {
	var total uint64
	for _, p := range prefixes {
		total += CountAddresses(p)
	}
	return total
}

// VerifyCoverage проверяет, что все исходные префиксы покрыты результирующим набором.
func VerifyCoverage(original, aggregated []netip.Prefix) bool {
	for _, orig := range original {
		covered := false
		for _, agg := range aggregated {
			if Contains(agg, orig) {
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
