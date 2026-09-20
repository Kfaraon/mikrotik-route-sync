package collectors

import (
	"context"
	"fmt"
	"net/netip"
	"time"
)

// WHOISCollector — метод для мелких сайтов без собственного источника:
// DNS → IP → ASN → все анонсированные префиксы ASN.
// Ограничен maxPrefixes для защиты от захвата лишнего (PROMPT II.2).
type WHOISCollector struct {
	resolver    ResolverAPI
	http        *HTTP
	cache       PrefixCache
	ttl         time.Duration
	domains     []string
	ips         []string
	maxPrefixes int
}

// NewWHOISCollector создаёт whois-коллектор.
func NewWHOISCollector(r ResolverAPI, h *HTTP, cache PrefixCache, ttl time.Duration, domains, ips []string, maxPrefixes int) *WHOISCollector {
	if maxPrefixes <= 0 {
		maxPrefixes = 100
	}
	return &WHOISCollector{
		resolver: r, http: h, cache: cache, ttl: ttl,
		domains: domains, ips: ips, maxPrefixes: maxPrefixes,
	}
}

func (c *WHOISCollector) Name() string { return "whois" }

// Collect: для каждого домена/IP определяет ASN и собирает его префиксы
// (не более maxPrefixes суммарно).
func (c *WHOISCollector) Collect(ctx context.Context, service string, opts Options) (*Result, error) {
	if c.resolver == nil || c.http == nil {
		return nil, fmt.Errorf("whois: resolver/http not configured")
	}

	var addresses []string
	for _, domain := range c.domains {
		ips, err := c.resolver.ResolveDomain(ctx, domain)
		if err != nil {
			continue
		}
		addresses = append(addresses, ips...)
	}
	addresses = append(addresses, c.ips...)

	if len(addresses) == 0 {
		return nil, fmt.Errorf("whois: no addresses resolved for service %s", service)
	}

	asns := map[int]bool{}
	for _, ipStr := range addresses {
		asn, err := c.resolver.ASNByIP(ctx, ipStr)
		if err != nil {
			continue
		}
		asns[asn] = true
	}
	if len(asns) == 0 {
		return nil, fmt.Errorf("whois: cannot determine ASN for service %s", service)
	}

	seen := map[netip.Prefix]bool{}
	var all []netip.Prefix
	for asn := range asns {
		lines, err := getASPrefixes(ctx, c.http, c.cache, c.ttl, asn)
		if err != nil {
			continue
		}
		for _, p := range parsePrefixLines(lines) {
			if seen[p] {
				continue
			}
			seen[p] = true
			all = append(all, p)
			if len(all) >= c.maxPrefixes {
				break
			}
		}
		if len(all) >= c.maxPrefixes {
			break
		}
	}

	all = filterPrefixes(all, opts)
	if len(all) == 0 {
		return nil, fmt.Errorf("whois: no prefixes found for service %s", service)
	}
	return &Result{
		Prefixes: all,
		Source:   "whois/cymru+bgp.tools",
		Method:   "whois",
	}, nil
}
