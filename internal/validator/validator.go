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
	out, _, err := v.ValidateWithStats(raw, ov)
	return out, err
}

// normalizeFilterList — normalize для exclude/include_only с fail-closed
// на отсев предфильтра. Запись фильтра, не пережившая предфильтр
// (reserved/IPv6/шире /8), никогда не совпадёт ни с чем пережившим его же —
// т.е. всегда no-op. Молча глотать её опасно: пользователь думает, что
// исключение работает, а оно проигнорировано. Поэтому — ошибка с именем
// записи, а не silent drop (в духе существующего fail-closed на опечатки).
func normalizeFilterList(raw []string, what string) ([]netip.Prefix, error) {
	out, pre, err := aggregator.NormalizeAllCounted(raw)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", what, err)
	}
	if pre.Total() > 0 {
		return nil, fmt.Errorf("%s entries silently ignored by pre-filter (ipv6=%d too_broad=%d blocked=%d): %s (filters must be valid public IPv4 prefixes)",
			what, pre.IPv6, pre.TooBroad, pre.Blocked,
			strings.Join(preFilterOffenders(raw), "; "))
	}
	return out, nil
}

// preFilterOffenders — записи raw, не пережившие предфильтр, с причинами.
// Второй проход только по error-пути: дёшево и даёт точные имена в ошибке.
func preFilterOffenders(raw []string) []string {
	var out []string
	for _, s := range raw {
		t := strings.TrimSpace(s)
		if t == "" {
			continue
		}
		p, err := netip.ParsePrefix(t)
		if err != nil {
			continue // parse-ошибки уже fail-closed отдельно, с деталями
		}
		if verr := aggregator.Validate(p, nil); verr != nil {
			out = append(out, fmt.Sprintf("%s [%v]", t, verr))
		}
	}
	return out
}

// RejectStats — счётчик префиксов, отброшенных валидацией, по причинам.
// Раньше отсев был невидимым (молчаливый continue): частичная потеря
// (например, все /32 при запрете хостов) диагностировалась только по
// пустому итогу. Теперь breakdown виден в логе и результате синка.
type RejectStats struct {
	IPv6        int `json:"ipv6"`
	Blocked     int `json:"blocked"`
	TooBroad    int `json:"too_broad"`
	HostRoute   int `json:"host_routes"`
	Excluded    int `json:"excluded"`
	NotIncluded int `json:"not_included"`
}

// Total — всего отброшено.
func (r RejectStats) Total() int {
	return r.IPv6 + r.Blocked + r.TooBroad + r.HostRoute + r.Excluded + r.NotIncluded
}

// ValidateWithStats — Validate плюс breakdown отсева.
func (v Validator) ValidateWithStats(raw []string, ov config.ServiceOverride) ([]netip.Prefix, RejectStats, error) {
	var rej RejectStats
	if len(raw) == 0 {
		return nil, rej, fmt.Errorf("collector returned zero prefixes")
	}
	// Считающий normalize: отсев предфильтра (IPv6/широкие/reserved)
	// раньше был невидимым — мержим в общую статистику.
	norm, pre, err := aggregator.NormalizeAllCounted(raw)
	if err != nil {
		return nil, rej, fmt.Errorf("normalize: %w", err)
	}
	rej.IPv6 += pre.IPv6
	rej.TooBroad += pre.TooBroad
	rej.Blocked += pre.Blocked
	// Fail-closed: опечатка в exclude/include не должна молча игнорироваться
	// (иначе исключённый диапазон останется в списке).
	excludes, err := normalizeFilterList(ov.Exclude, "overrides.exclude")
	if err != nil {
		return nil, rej, err
	}
	includes, err := normalizeFilterList(ov.IncludeOnly, "overrides.include_only")
	if err != nil {
		return nil, rej, err
	}

	allowHosts := v.Safety.AllowHostRoutesEnabled()
	out := make([]netip.Prefix, 0, len(norm))
	for _, p := range norm {
		if !p.Addr().Is4() {
			rej.IPv6++
			continue
		}
		if overlapsBlocked(p) {
			rej.Blocked++
			continue
		}
		if p.Bits() < v.Safety.MinPrefixV4 {
			rej.TooBroad++
			continue
		}
		if !allowHosts && p.Bits() == 32 {
			rej.HostRoute++
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
			rej.Excluded++
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
				rej.NotIncluded++
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
		return nil, rej, fmt.Errorf("validated prefix count %d exceeds limit %d", len(out), limit)
	}
	if len(out) == 0 {
		msg := fmt.Sprintf("all prefixes rejected by validation (ipv6=%d blocked=%d too_broad=%d host_routes=%d excluded=%d not_included=%d)",
			rej.IPv6, rej.Blocked, rej.TooBroad, rej.HostRoute, rej.Excluded, rej.NotIncluded)
		if rej.HostRoute > 0 && !allowHosts {
			msg += "; host routes dropped: set safety.allow_host_routes=true (dynamic returns only /32)"
		}
		return nil, rej, fmt.Errorf("%s", msg)
	}
	return out, rej, nil
}

// BroadPrefixes возвращает строковые префиксы шире порога (маска < bits).
// Warn-инструмент для VPN-точности: широкие префиксы гонят лишний трафик.
// Официальные списки CDN намеренно сюда не попадают (см. вызовы) —
// их широта курирована провайдером.
func BroadPrefixes(in []netip.Prefix, bits int) []string {
	var out []string
	for _, p := range in {
		if p.Bits() < bits {
			out = append(out, p.String())
		}
	}
	return out
}

// SanitizeComment запрещает characters, ломающие RouterOS-комментарии
// (PROMPT XI.9): двойные кавычки, обратный слэш, переводы строк.
func SanitizeComment(s string) error {
	if strings.ContainsAny(s, "\"\\\n\r") {
		return fmt.Errorf("unsafe service/comment characters")
	}
	return nil
}
