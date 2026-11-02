package collectors

import (
	"context"
	"fmt"
	"net/netip"
)

// PresetCollector — сбор строго по доменам привязанного пресета: DNS → IPv4
// (/32), плюс необязательные официальные списки доп. CDN (also_cdn).
//
// В отличие от DynamicCollector НЕ подмешивает диапазоны Google: пресет —
// это ровно его домены и явно указанные also_cdn-источники.
type PresetCollector struct {
	resolver ResolverAPI
	http     *HTTP
	domains  []string
	alsoCDN  []int // ASN CDN-источников, которые нужно дополнительно опросить
}

// NewPresetCollector создаёт preset-коллектор. http может быть nil, если
// also_cdn не используется.
func NewPresetCollector(r ResolverAPI, h *HTTP, domains []string, alsoCDN []int) *PresetCollector {
	return &PresetCollector{resolver: r, http: h, domains: domains, alsoCDN: alsoCDN}
}

func (c *PresetCollector) Name() string { return "preset" }

// Collect резолвит каждый домен в IPv4-адреса и складывает их как /32.
// Домены, которые не резолвятся, пропускаются молча (как в dynamic).
// also_cdn-источники (официальные списки CDN) добавляются, если заданы.
func (c *PresetCollector) Collect(ctx context.Context, service string, opts Options) (*Result, error) {
	var all []netip.Prefix

	for i, domain := range c.domains {
		Report(ctx, fmt.Sprintf("DNS: %s (%d/%d)", domain, i+1, len(c.domains)), i+1, len(c.domains))
		ips, err := c.resolver.ResolveDomain(ctx, domain)
		if err != nil {
			continue
		}
		for _, ipStr := range ips {
			addr, err := netip.ParseAddr(ipStr)
			if err != nil || !addr.Is4() {
				continue
			}
			all = append(all, netip.PrefixFrom(addr, 32))
		}
	}

	// Дополнительные CDN-источники (also_cdn): в отличие от dynamic диапазоны
	// Google не подмешиваются, но явно указанные CDN — да.
	for i, asn := range c.alsoCDN {
		if c.http == nil {
			break
		}
		Report(ctx, fmt.Sprintf("доп. CDN AS%d (%d/%d)", asn, i+1, len(c.alsoCDN)), i+1, len(c.alsoCDN))
		if r, err := NewCDNCollector(asn, c.http).Collect(ctx, service, Options{}); err == nil {
			all = append(all, r.Prefixes...)
		}
	}

	if len(all) == 0 {
		return nil, fmt.Errorf("preset: no prefixes resolved for service %s", service)
	}

	return &Result{
		Prefixes: all,
		Source:   "dns",
		Method:   "preset",
	}, nil
}
