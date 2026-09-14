package collectors

import (
	"context"
	"fmt"
	"net"
	"net/netip"
)

// DynamicCollector — резолвит домены сервиса и превращает A-записи в /32.
// Для google/youtube лучше использовать CDN-источник, но fallback — здесь.
type DynamicCollector struct{}

func (d *DynamicCollector) Name() string { return "dynamic" }

func (d *DynamicCollector) Collect(ctx context.Context, service string, opts Options) (*Result, error) {
	if len(opts.Domains) == 0 {
		return nil, fmt.Errorf("dynamic collector requires domains")
	}
	var out []netip.Prefix
	for _, dom := range opts.Domains {
		ips, err := net.DefaultResolver.LookupNetIP(ctx, "ip4", dom)
		if err != nil {
			continue
		}
		for _, ip := range ips {
			out = append(out, netip.PrefixFrom(ip, 32))
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("no A records for %v", opts.Domains)
	}
	return &Result{Prefixes: out, Source: fmt.Sprintf("dns:%v", opts.Domains), Method: "dynamic"}, nil
}package collectors

import (
    "context"

    "github.com/Kfaraon/mikrotik-route-sync/internal/resolver"
)

type DynamicCollector struct {
    resolver *resolver.Resolver
    domains  []string
}

func NewDynamicCollector(r *resolver.Resolver, domains []string) *DynamicCollector {
    return &DynamicCollector{resolver: r, domains: domains}
}

func (c *DynamicCollector) Collect(ctx context.Context, _ string) ([]string, error) {
    var out []string
    seen := map[string]struct{}{}
    for _, d := range c.domains {
        ips, err := c.resolver.ResolveDomain(ctx, d)
        if err != nil {
            continue
        }
        for _, ip := range ips {
            key := ip + "/32"
            if _, ok := seen[key]; ok {
                continue
            }
            seen[key] = struct{}{}
            out = append(out, key)
        }
    }
    return out, nil
}
