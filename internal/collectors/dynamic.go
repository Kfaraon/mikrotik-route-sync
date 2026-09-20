package collectors

import (
	"context"
	"fmt"
	"net/netip"
)

// DynamicCollector — DNS-резолвинг доменов + официальный Google JSON.
// Используется для сервисов с динамической инфраструктурой (Google, YouTube, Telegram).
type DynamicCollector struct {
	resolver ResolverAPI
	http     *HTTP
	domains  []string
	alsoCDN  []int // ASN CDN-источников, которые нужно дополнительно опросить
}

// NewDynamicCollector создаёт dynamic-коллектор.
func NewDynamicCollector(r ResolverAPI, h *HTTP, domains []string, alsoCDN []int) *DynamicCollector {
	return &DynamicCollector{resolver: r, http: h, domains: domains, alsoCDN: alsoCDN}
}

func (c *DynamicCollector) Name() string { return "dynamic" }

// Collect резолвит домены в IPv4-хост-адреса (/32) и дополняет официальными JSON-списками.
// IPv6 не поддерживается проектом.
func (c *DynamicCollector) Collect(ctx context.Context, service string, opts Options) (*Result, error) {
	var all []netip.Prefix

	for _, domain := range c.domains {
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

	// Официальные Google-диапазоны
	if c.http != nil {
		if g, err := fetchGoogleRanges(ctx, c.http); err == nil {
			all = append(all, g...)
		}
	}

	// Дополнительные CDN-источники (also_cdn)
	for _, asn := range c.alsoCDN {
		if r, err := NewCDNCollector(asn, c.http).Collect(ctx, service, Options{}); err == nil {
			all = append(all, r.Prefixes...)
		}
	}

	if len(all) == 0 {
		return nil, fmt.Errorf("dynamic: no prefixes resolved for service %s", service)
	}

	return &Result{
		Prefixes: all,
		Source:   "dns+google_json",
		Method:   "dynamic",
	}, nil
}

// fetchGoogleRanges — официальный список IPv4-префиксов Google.
func fetchGoogleRanges(ctx context.Context, h *HTTP) ([]netip.Prefix, error) {
	var data struct {
		Prefixes []struct {
			IPv4Prefix string `json:"ipv4Prefix"`
		} `json:"prefixes"`
	}
	if err := h.FetchJSON(ctx, "https://www.gstatic.com/ipranges/goog.json", &data); err != nil {
		return nil, err
	}
	var out []netip.Prefix
	for _, p := range data.Prefixes {
		if p.IPv4Prefix != "" {
			if prefix, err := netip.ParsePrefix(p.IPv4Prefix); err == nil {
				out = append(out, prefix.Masked())
			}
		}
	}
	return out, nil
}
