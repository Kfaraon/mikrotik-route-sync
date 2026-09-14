package collectors

import (
	"context"
	"net/netip"

	"github.com/Kfaraon/mikrotik-route-sync/internal/resolver"
)

type DynamicCollector struct {
	resolver *resolver.Resolver
	domains  []string
}

func NewDynamicCollector(r *resolver.Resolver, domains []string) Collector {
	return &DynamicCollector{resolver: r, domains: domains}
}

func (c *DynamicCollector) Name() string { return "dynamic" }

func (c *DynamicCollector) Collect(ctx context.Context, service string, opts Options) (*Result, error) {
	var netPrefixes []netip.Prefix

	for _, domain := range c.domains {
		ips, err := c.resolver.ResolveDomain(ctx, domain)
		if err != nil {
			continue
		}

		for _, ip := range ips {
			if prefix, err := netip.ParsePrefix(ip + "/32"); err == nil {
				netPrefixes = append(netPrefixes, prefix)
			}
		}
	}

	return &Result{
		Prefixes: netPrefixes,
		Source:   "dns",
		Method:   "dynamic",
	}, nil
}
