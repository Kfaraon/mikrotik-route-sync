package collectors

import (
	"context"
	"fmt"
	"net/netip"

	"github.com/Kfaraon/mikrotik-route-sync/internal/config"
)

// ASNCollector собирает префиксы для конкретного ASN.
//
// Использует два источника:
// 1. BGPView API (основной) — быстрый, надежный
// 2. RIPEstat API (fallback) — официальный источник от RIPE NCC
type ASNCollector struct {
	asn  int
	http *HTTP
}

// NewASNCollector создаёт коллектор для ASN.
func NewASNCollector(asn int, http *HTTP) *ASNCollector {
	return &ASNCollector{asn: asn, http: http}
}

// Collect возвращает анонсированные префиксы для ASN.
//
// Алгоритм:
// 1. Пытаемся получить префиксы через BGPView
// 2. Если ошибка или пустой результат → пробуем RIPEstat
// 3. Если оба источника не работают → возвращаем ошибку
// 4. Применяем исключения (exclude) из конфига
// 5. Маскируем префиксы (нормализуем хостовые биты)
func (c *ASNCollector) Collect(ctx context.Context, service string, opts Options) (*Result, error) {
	var prefixes []string
	var source string
	var lastErr error

	// ──────────────────────────────────────────────────────────────
	// Шаг 1: Пробуем BGPView (основной источник)
	// ──────────────────────────────────────────────────────────────
	prefixes, err := BGPViewASN(ctx, c.http, fmt.Sprintf("AS%d", c.asn))
	if err != nil {
		lastErr = fmt.Errorf("bgpview: %w", err)
	} else if len(prefixes) == 0 {
		lastErr = fmt.Errorf("bgpview: no prefixes")
	} else {
		source = "bgpview"
	}

	// ──────────────────────────────────────────────────────────────
	// Шаг 2: Если BGPView не сработал → пробуем RIPEstat (fallback)
	// ──────────────────────────────────────────────────────────────
	if source == "" {
		prefixes, err = RIPEstatASN(ctx, c.http, c.asn)
		if err != nil {
			lastErr = fmt.Errorf("ripestat: %w", err)
		} else if len(prefixes) == 0 {
			lastErr = fmt.Errorf("ripestat: no prefixes")
		} else {
			source = "ripestat"
		}
	}

	// ──────────────────────────────────────────────────────────────
	// Шаг 3: Оба источника не сработали → ошибка
	// ──────────────────────────────────────────────────────────────
	if source == "" {
		return nil, fmt.Errorf("all ASN sources failed for AS%d: %w", c.asn, lastErr)
	}

	// ──────────────────────────────────────────────────────────────
	// Шаг 4: Фильтрация и нормализация
	// ──────────────────────────────────────────────────────────────
	excludeSet := make(map[netip.Prefix]struct{}, len(opts.Exclude))
	for _, e := range opts.Exclude {
		if p, err := netip.ParsePrefix(e); err == nil {
			excludeSet[p.Masked()] = struct{}{}
		}
	}

	includeOnly := len(opts.IncludeOnly) > 0
	includeSet := make(map[netip.Prefix]struct{}, len(opts.IncludeOnly))
	if includeOnly {
		for _, inc := range opts.IncludeOnly {
			if p, err := netip.ParsePrefix(inc); err == nil {
				includeSet[p.Masked()] = struct{}{}
			}
		}
	}

	out := make([]netip.Prefix, 0, len(prefixes))
	for _, s := range prefixes {
		p, err := netip.ParsePrefix(s)
		if err != nil {
			continue
		}
		p = p.Masked()
		if _, ok := excludeSet[p]; ok {
			continue
		}
		if includeOnly {
			if _, ok := includeSet[p]; !ok {
				continue
			}
		}
		out = append(out, p)
	}

	if len(out) == 0 {
		return nil, fmt.Errorf("no valid prefixes after filtering for AS%d", c.asn)
	}

	return &Result{
		Prefixes: out,
		Source:   source,
		Method:   "asn",
	}, nil
}

// Options содержит параметры фильтрации.
type Options struct {
	Exclude     []string
	IncludeOnly []string
}
