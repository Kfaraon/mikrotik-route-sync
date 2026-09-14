package collectors

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/netip"
	"time"

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
	var prefixes []netip.Prefix

	// Resolve domains
	for _, domain := range c.domains {
		ips, err := c.resolver.ResolveDomain(ctx, domain)
		if err != nil {
			continue
		}
		for _, ip := range ips {
			if addr, err := netip.ParseAddr(ip); err == nil {
				// Convert single IP to /32
				prefixes = append(prefixes, netip.PrefixFrom(addr, 32))
			}
		}
	}

	// Also fetch official Google ranges
	googleRanges, err := fetchGoogleRanges(ctx)
	if err == nil {
		prefixes = append(prefixes, googleRanges...)
	}

	return &Result{
		Prefixes: prefixes,
		Source:   "dns+google_json",
		Method:   "dynamic",
	}, nil
}

func fetchGoogleRanges(ctx context.Context) ([]netip.Prefix, error) {
	client := &http.Client{Timeout: 30 * time.Second}
	req, err := http.NewRequestWithContext(ctx, "GET", "https://www.gstatic.com/ipranges/goog.json", nil)
	if err != nil {
		return nil, err
	}

	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	var data struct {
		Prefixes []struct {
			IPv4Prefix string `json:"ipv4Prefix"`
		} `json:"prefixes"`
	}
	if err := json.Unmarshal(body, &data); err != nil {
		return nil, err
	}

	var prefixes []netip.Prefix
	for _, p := range data.Prefixes {
		if p.IPv4Prefix != "" {
			if prefix, err := netip.ParsePrefix(p.IPv4Prefix); err == nil {
				prefixes = append(prefixes, prefix.Masked())
			}
		}
	}
	return prefixes, nil
}
