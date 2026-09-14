package collectors

import (
	"context"
	"net/netip"

	"github.com/Kfaraon/mikrotik-route-sync/internal/resolver"
)

type WHOISCollector struct {
	resolver *resolver.Resolver
	domains  []string
	maxASN   int
}

func NewWHOISCollector(r *resolver.Resolver, domains []string, maxASN int) Collector {
	return &WHOISCollector{resolver: r, domains: domains, maxASN: maxASN}
}

func (c *WHOISCollector) Name() string { return "whois" }

func (c *WHOISCollector) Collect(ctx context.Context, service string, opts Options) (*Result, error) {
	var netPrefixes []netip.Prefix

	for _, domain := range c.domains {
		ips, err := c.resolver.ResolveDomain(ctx, domain)
		if err != nil || len(ips) == 0 {
			continue
		}

		asn, err := c.resolver.GetASN(ctx, ips[0])
		if err != nil || asn == 0 {
			continue
		}

		prefixes, err := c.resolver.GetASPrefixes(ctx, asn)
		if err != nil {
			continue
		}

		count := 0
		for _, p := range prefixes {
			if count >= c.maxASN {
				break
			}
			if prefix, err := netip.ParsePrefix(p); err == nil {
				netPrefixes = append(netPrefixes, prefix)
				count++
			}
		}
	}

	return &Result{
		Prefixes: netPrefixes,
		Source:   "whois/bgpview",
		Method:   "whois",
	}, nil
}
