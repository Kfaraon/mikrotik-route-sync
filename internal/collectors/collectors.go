// Package collectors — источники сырых IP-диапазонов.
//
// Collector только собирает данные из источника (не валидирует и не агрегирует —
// это ответственность validator/aggregator, PROMPT X.10.1).
package collectors

import (
	"context"
	"net/netip"
	"time"
)

// Collector — источник сетей для одного метода сбора.
type Collector interface {
	Name() string
	Collect(ctx context.Context, service string, opts Options) (*Result, error)
}

// Result — сырой результат сбора (до валидации).
type Result struct {
	Prefixes []netip.Prefix `json:"prefixes"`
	Source   string         `json:"source"`
	Method   string         `json:"method"`
}

// Options — параметры фильтрации из overrides сервиса.
type Options struct {
	Exclude     []string
	IncludeOnly []string
}

// ResolverAPI — интерфейс DNS/ASN-резолвинга (реализуется *resolver.Resolver).
// Определяется здесь, чтобы collectors не импортировал resolver.
type ResolverAPI interface {
	ResolveDomain(ctx context.Context, domain string) ([]string, error)
	ASNByIP(ctx context.Context, ip string) (int, error)
}

// PrefixCache — кэш префиксов по ASN (реализуется storage.Cache).
type PrefixCache interface {
	GetPrefixes(asn int) ([]string, bool)
	SetPrefixes(asn int, prefixes []string, ttl time.Duration) error
}

// allowedByInclude проверяет include_only с семантикой вложенности (как
// валидатор, aggregator.ContainsPrefix): префикс допускается, если он
// полностью содержится хотя бы в одном include_only. Точного совпадения
// недостаточно — иначе include 1.2.3.0/24 отбрасывал бы 1.2.3.0/25 у
// коллектора, хотя валидатор его пропускает (PROMPT II.1).
func allowedByInclude(p netip.Prefix, include []netip.Prefix) bool {
	for _, in := range include {
		if in.Bits() <= p.Bits() && in.Contains(p.Addr()) {
			return true
		}
	}
	return false
}

// filterPrefixes применяет exclude/include_only фильтры к набору префиксов.
func filterPrefixes(in []netip.Prefix, opts Options) []netip.Prefix {
	excludeSet := parsePrefixSet(opts.Exclude)
	includeOnly := len(opts.IncludeOnly) > 0
	include := parsePrefixList(opts.IncludeOnly)

	out := make([]netip.Prefix, 0, len(in))
	for _, p := range in {
		p = p.Masked()
		if _, ok := excludeSet[p]; ok {
			continue
		}
		if includeOnly && !allowedByInclude(p, include) {
			continue
		}
		out = append(out, p)
	}
	return out
}

func parsePrefixSet(raw []string) map[netip.Prefix]struct{} {
	set := make(map[netip.Prefix]struct{}, len(raw))
	for _, s := range raw {
		if p, err := netip.ParsePrefix(s); err == nil {
			set[p.Masked()] = struct{}{}
		}
	}
	return set
}

// parsePrefixList — те же валидные IPv4-префиксы, но списком (для проверки
// вложенности без выделения map).
func parsePrefixList(raw []string) []netip.Prefix {
	out := make([]netip.Prefix, 0, len(raw))
	for _, s := range raw {
		if p, err := netip.ParsePrefix(s); err == nil && p.Addr().Is4() {
			out = append(out, p.Masked())
		}
	}
	return out
}

// parsePrefixLines конвертирует строки вида "1.2.3.0/24" в []netip.Prefix.
// Непарсабельные строки и IPv6-префиксы отбрасываются (проект — только IPv4).
func parsePrefixLines(lines []string) []netip.Prefix {
	out := make([]netip.Prefix, 0, len(lines))
	seen := make(map[netip.Prefix]bool)
	for _, s := range lines {
		p, err := netip.ParsePrefix(s)
		if err != nil || !p.Addr().Is4() {
			continue
		}
		p = p.Masked()
		if !seen[p] {
			seen[p] = true
			out = append(out, p)
		}
	}
	return out
}
