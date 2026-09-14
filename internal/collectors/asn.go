package collectors

import (
	"context"
	"fmt"
	"net/netip"

	"github.com/Kfaraon/mikrotik-route-sync/internal/resolver"
)

type ASNCollector struct {
	resolver *resolver.Resolver
	asn      int
}

func NewASNCollector(r *resolver.Resolver, asn int) Collector {
	return &ASNCollector{resolver: r, asn: asn}
}

func (c *ASNCollector) Name() string { return "asn" }

func (c *ASNCollector) Collect(ctx context.Context, service string, opts Options) (*Result, error) {
	prefixes, err := c.resolver.GetASPrefixes(ctx, c.asn)
	if err != nil {
		return nil, fmt.Errorf("get AS prefixes: %w", err)
	}

	var netPrefixes []netip.Prefix
	excludeSet := make(map[netip.Prefix]bool)
	for _, ex := range opts.Exclude {
		excludeSet[ex.Masked()] = true
	}

	for _, p := range prefixes {
		prefix, err := netip.ParsePrefix(p)
		if err != nil {
			continue
		}
		prefix = prefix.Masked()

		// Skip excluded
		if excludeSet[prefix] {
			continue
		}

		// Validate prefix belongs to ASN (optional WHOIS check)
		// For performance, we trust BGPView data
		netPrefixes = append(netPrefixes, prefix)
	}

	return &Result{
		Prefixes: netPrefixes,
		Source:   "bgpview",
		Method:   "asn",
	}, nil
}
