package collectors

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
