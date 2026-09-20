package collectors

import (
	"context"
	"fmt"
	"net/netip"
	"strconv"
	"strings"
	"time"
)

// bgpToolsTableURL — публичный дамп BGP-таблицы bgp.tools (формат: "CIDR ASN"
// на строку). Используется вместо закрытого/недоступного BGPView.
// bgp.tools НЕ требует API-ключ, но запрещает скрапинг HTML и требует
// описательный User-Agent (см. NewHTTP) и бережное обращение (кэш по TTL).
const bgpToolsTableURL = "https://bgp.tools/table.txt"

// bgpToolsDownloadTimeout — разумный бюджет на загрузку полного дампа (~30 МБ).
const bgpToolsDownloadTimeout = 120 * time.Second

// bgpToolsLineMatch разбирает строку дампа bgp.tools ("CIDR ASN [ASN...]")
// и возвращает prefix, если он IPv4 и один из полей ASN равен target.
func bgpToolsLineMatch(line, target string) (string, bool) {
	fields := strings.Fields(line)
	if len(fields) < 2 {
		return "", false
	}
	prefix := fields[0]
	matched := false
	for _, f := range fields[1:] {
		if strings.TrimLeft(strings.ToLower(f), "as") == target {
			matched = true
			break
		}
	}
	if !matched {
		return "", false
	}
	if p, err := netip.ParsePrefix(prefix); err != nil || !p.Addr().Is4() {
		return "", false
	}
	return prefix, true
}

// BGPToolsASN возвращает анонсированные IPv4-префиксы ASN из дампа таблицы
// bgp.tools. Потоковая фильтрация: тело не буферизуется целиком.
func BGPToolsASN(ctx context.Context, h *HTTP, asn int) ([]string, error) {
	if asn <= 0 {
		return nil, fmt.Errorf("invalid ASN %d", asn)
	}
	ctx, cancel := context.WithTimeout(ctx, bgpToolsDownloadTimeout)
	defer cancel()

	target := strconv.Itoa(asn)
	out := make([]string, 0, 64)
	err := h.StreamLines(ctx, bgpToolsTableURL, func(line string) error {
		if prefix, ok := bgpToolsLineMatch(line, target); ok {
			out = append(out, prefix)
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("bgp.tools: %w", err)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("bgp.tools: no prefixes for AS%d", asn)
	}
	return out, nil
}

// getASPrefixes — основной источник bgp.tools (таблица BGP), fallback RIPEstat,
// с кэшированием результата по ASN в bbolt на TTL конфига.
func getASPrefixes(ctx context.Context, h *HTTP, cache PrefixCache, ttl time.Duration, asn int) ([]string, error) {
	if cache != nil {
		if cached, ok := cache.GetPrefixes(asn); ok && len(cached) > 0 {
			return cached, nil
		}
	}

	prefixes, toolsErr := BGPToolsASN(ctx, h, asn)
	if toolsErr != nil {
		var ripeErr error
		prefixes, ripeErr = RIPEstatASN(ctx, h, asn)
		if ripeErr != nil {
			return nil, fmt.Errorf("all ASN sources failed for AS%d: bgp.tools=%v; ripestat=%v", asn, toolsErr, ripeErr)
		}
	}

	if cache != nil && len(prefixes) > 0 {
		if ttl <= 0 {
			ttl = 24 * time.Hour
		}
		_ = cache.SetPrefixes(asn, prefixes, ttl)
	}
	return prefixes, nil
}

// ASNCollector собирает анонсированные IPv4-префиксы конкретного ASN.
//
// bgp.tools таблица (основной) → RIPEstat (fallback), с кэшированием в bbolt.
type ASNCollector struct {
	asn   int
	http  *HTTP
	cache PrefixCache
	ttl   time.Duration
}

// NewASNCollector создаёт коллектор для ASN.
func NewASNCollector(asn int, h *HTTP, cache PrefixCache, ttl time.Duration) *ASNCollector {
	return &ASNCollector{asn: asn, http: h, cache: cache, ttl: ttl}
}

func (c *ASNCollector) Name() string { return "asn" }

// Collect возвращает префиксы ASN с применением exclude/include_only.
func (c *ASNCollector) Collect(ctx context.Context, service string, opts Options) (*Result, error) {
	lines, err := getASPrefixes(ctx, c.http, c.cache, c.ttl, c.asn)
	if err != nil {
		return nil, err
	}

	out := filterPrefixes(parsePrefixLines(lines), opts)
	if len(out) == 0 {
		return nil, fmt.Errorf("no valid prefixes after filtering for AS%d", c.asn)
	}

	return &Result{
		Prefixes: out,
		Source:   "bgp.tools/ripestat",
		Method:   "asn",
	}, nil
}
