package collectors

import (
	"context"
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
		return nil, err
	}

	var netPrefixes []netip.Prefix
	for _, p := range prefixes {
		if prefix, err := netip.ParsePrefix(p); err == nil {
			netPrefixes = append(netPrefixes, prefix)
		}
	}

	return &Result{
		Prefixes: netPrefixes,
		Source:   "bgpview",
		Method:   "asn",
	}, nil
}
