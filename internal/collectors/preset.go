package collectors

import (
	"context"
	"fmt"
	"net/netip"
)

// PresetCollector — сбор строго по доменам привязанного пресета: DNS → IPv4
// (/32). В отличие от DynamicCollector НИКОГДА не подмешивает официальные
// диапазоны Google и доп. CDN-источники (also_cdn): пресет — это ровно его
// список доменов, и ничего кроме.
type PresetCollector struct {
	resolver ResolverAPI
	domains  []string
}

// NewPresetCollector создаёт preset-коллектор.
func NewPresetCollector(r ResolverAPI, domains []string) *PresetCollector {
	return &PresetCollector{resolver: r, domains: domains}
}

func (c *PresetCollector) Name() string { return "preset" }

// Collect резолвит каждый домен в IPv4-адреса и складывает их как /32.
// Домены, которые не резолвятся, пропускаются молча (как в dynamic).
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

	if len(all) == 0 {
		return nil, fmt.Errorf("preset: no prefixes resolved for service %s", service)
	}

	return &Result{
		Prefixes: all,
		Source:   "dns",
		Method:   "preset",
	}, nil
}
