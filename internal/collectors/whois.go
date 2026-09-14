package collectors

import (
	"context"
	"fmt"
	"net/netip"

	"github.com/Kfaraon/mikrotik-route-sync/internal/resolver"
)

type WHOISCollector struct {
	resolver   *resolver.Resolver
	domains    []string
	maxPrefixes int
}

func NewWHOISCollector(r *resolver.Resolver, domains []string, maxPrefixes int) Collector {
	return &WHOISCollector{resolver: r, domains: domains, maxPrefixes: maxPrefixes}
}

func (c *WHOISCollector) Name() string { return "whois" }

func (c *WHOISCollector) Collect(ctx context.Context, service string, opts Options) (*Result, error) {
	var allPrefixes []netip.Prefix
	seen := make(map[netip.Prefix]bool)

	excludeSet := make(map[netip.Prefix]bool)
	for _, ex := range opts.Exclude {
		excludeSet[ex.Masked()] = true
	}

	for _, domain := range c.domains {
		ips, err := c.resolver.ResolveDomain(ctx, domain)
		if err != nil || len(ips) == 0 {
			continue
		}

		// Get ASN from first IP
		asn, err := c.resolver.ASNByIP(ctx, ips[0])
		if err != nil {
			continue
		}

		// Get all prefixes for this ASN
		prefixes, err := c.resolver.GetASPrefixes(ctx, asn)
		if err != nil {
			continue
		}

		for _, p := range prefixes {
			prefix, err := netip.ParsePrefix(p)
			if err != nil {
				continue
			}
			prefix = prefix.Masked()

			if excludeSet[prefix] {
				continue
			}
			if seen[prefix] {
				continue
			}
			seen[prefix] = true
			allPrefixes = append(allPrefixes, prefix)

			if len(allPrefixes) >= c.maxPrefixes {
				break
			}
		}

		if len(allPrefixes) >= c.maxPrefixes {
			break
		}
	}

	if len(allPrefixes) == 0 {
		return nil, fmt.Errorf("no prefixes found for service %s", service)
	}

	return &Result{
		Prefixes: allPrefixes,
		Source:   "whois",
		Method:   "whois",
	}, nil
}
