package collectors

import (
    "context"

    "github.com/Kfaraon/mikrotik-route-sync/internal/resolver"
)

type WHOISCollector struct {
    resolver *resolver.Resolver
    domains  []string
    maxASN   int
}

func NewWHOISCollector(r *resolver.Resolver, domains []string, maxASN int) *WHOISCollector {
    if maxASN <= 0 {
        maxASN = 200
    }
    return &WHOISCollector{resolver: r, domains: domains, maxASN: maxASN}
}

func (c *WHOISCollector) Collect(ctx context.Context, _ string) ([]string, error) {
    out := map[string]struct{}{}
    for _, d := range c.domains {
        ips, err := c.resolver.ResolveDomain(ctx, d)
        if err != nil {
            continue
        }
        for _, ip := range ips {
            asn, err := c.resolver.ASNByIP(ctx, ip)
            if err != nil {
                out[ip+"/32"] = struct{}{}
                continue
            }
            prefixes, err := c.resolver.PrefixesByASN(ctx, asn)
            if err != nil || len(prefixes) > c.maxASN {
                out[ip+"/32"] = struct{}{}
                continue
            }
            for _, p := range prefixes {
                out[p] = struct{}{}
            }
        }
    }
    result := make([]string, 0, len(out))
    for k := range out {
        result = append(result, k)
    }
    return result, nil
}
