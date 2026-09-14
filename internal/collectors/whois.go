package collectors

import (
	"context"
	"fmt"
	"net"
	"net/netip"

	"yourmodule/internal/resolver"
)

// WHOISCollector — DNS → ASN → все префиксы ASN (с лимитом).
type WHOISCollector struct {
	Resolver *resolver.Resolver
}

func (w *WHOISCollector) Name() string { return "whois" }

func (w *WHOISCollector) Collect(ctx context.Context, service string, opts Options) (*Result, error) {
	if len(opts.Domains) == 0 {
		return nil, fmt.Errorf("whois collector requires domains")
	}
	var asns []int
	for _, d := range opts.Domains {
		ips, err := net.DefaultResolver.LookupNetIP(ctx, "ip4", d)
		if err != nil {
			continue
		}
		for _, ip := range ips {
			asn, err := w.Resolver.LookupASN(ctx, ip)
			if err == nil && asn > 0 {
				asns = append(asns, asn)
				break
			}
		}
	}
	if len(asns) == 0 {
		return nil, fmt.Errorf("no ASN found for %v", opts.Domains)
	}

	asnColl := &ASNCollector{}
	var all []netip.Prefix
	for _, asn := range asns {
		res, err := asnColl.Collect(ctx, service, Options{
			ASN:            asn,
			MaxASNPrefixes: opts.MaxASNPrefixes,
		})
		if err != nil {
			continue
		}
		all = append(all, res.Prefixes...)
	}
	return &Result{Prefixes: all, Source: "whois", Method: "whois"}, nil
}
