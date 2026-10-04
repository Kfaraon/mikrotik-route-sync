package validator

import (
	"fmt"
	"net/netip"
	"strings"

	"github.com/Kfaraon/mikrotik-route-sync/internal/aggregator"
	"github.com/Kfaraon/mikrotik-route-sync/internal/config"
)

type Validator struct {
	Safety config.SafetyConfig
}

// Project works with IPv4 only. IPv6 prefixes are rejected.

var blocked = mustPrefixes([]string{
	"0.0.0.0/8", "10.0.0.0/8", "100.64.0.0/10", "127.0.0.0/8",
	"169.254.0.0/16", "172.16.0.0/12", "192.0.0.0/24", "192.0.2.0/24",
	"192.88.99.0/24", "192.168.0.0/16", "198.18.0.0/15", "198.51.100.0/24",
	"203.0.113.0/24", "224.0.0.0/4", "240.0.0.0/4", "255.255.255.255/32",
})

func mustPrefixes(xs []string) []netip.Prefix {
	r := make([]netip.Prefix, 0, len(xs))
	for _, s := range xs {
		r = append(r, netip.MustParsePrefix(s))
	}
	return r
}

func overlapsBlocked(p netip.Prefix) bool {
	for _, b := range blocked {
		if b.Overlaps(p) {
			return true
		}
	}
	return false
}

func (v Validator) Validate(raw []string, ov config.ServiceOverride) ([]netip.Prefix, error) {
	if len(raw) == 0 {
		return nil, fmt.Errorf("collector returned zero prefixes")
	}
	norm, err := aggregator.NormalizeAll(raw)
	if err != nil {
		return nil, fmt.Errorf("normalize: %w", err)
	}
	// Fail-closed: опечатка в exclude/include не должна молча игнорироваться
	// (иначе исключённый диапазон останется в списке).
	excludes, err := aggregator.NormalizeAll(ov.Exclude)
	if err != nil {
		return nil, fmt.Errorf("overrides.exclude: %w", err)
	}
	includes, err := aggregator.NormalizeAll(ov.IncludeOnly)
	if err != nil {
		return nil, fmt.Errorf("overrides.include_only: %w", err)
	}

	out := make([]netip.Prefix, 0, len(norm))
	for _, p := range norm {
		if !p.Addr().Is4() {
			continue
		}
		if overlapsBlocked(p) {
			continue
		}
		if p.Bits() < v.Safety.MinPrefixV4 || (!v.Safety.AllowHostRoutes && p.Bits() == 32) {
			continue
		}
		// Уровень 5, exclude: исключение, покрывающее p целиком (включая
		// равенство), удаляет p; исключение внутри p вычитается из p —
		// остальные адреса сохраняются (пример PROMPT II.3: include
		// 1.2.3.0/24 + exclude 1.2.3.4/32 → /24 без одного хоста).
		excluded := false
		for _, x := range excludes {
			if aggregator.ContainsPrefix(x, p) {
				excluded = true
				break
			}
		}
		if excluded {
			continue
		}
		if len(includes) > 0 {
			ok := false
			for _, x := range includes {
				if aggregator.ContainsPrefix(x, p) {
					ok = true
					break
				}
			}
			if !ok {
				continue
			}
		}
		pieces := []netip.Prefix{p}
		for _, x := range excludes {
			if !aggregator.ContainsPrefix(p, x) {
				continue
			}
			next := make([]netip.Prefix, 0, len(pieces))
			for _, piece := range pieces {
				next = append(next, aggregator.SubtractPrefix(piece, x)...)
			}
			pieces = next
			if len(pieces) == 0 {
				break
			}
		}
		out = append(out, pieces...)
	}
	out = aggregator.RemoveContained(out)
	limit := ov.MaxPrefixes
	if limit == 0 {
		limit = 10000
	}
	if len(out) > limit {
		return nil, fmt.Errorf("validated prefix count %d exceeds limit %d", len(out), limit)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("all prefixes rejected by validation")
	}
	return out, nil
}

// SanitizeComment запрещает characters, ломающие RouterOS-комментарии
// (PROMPT XI.9): двойные кавычки, обратный слэш, переводы строк.
func SanitizeComment(s string) error {
	if strings.ContainsAny(s, "\"\\\n\r") {
		return fmt.Errorf("unsafe service/comment characters")
	}
	return nil
}
