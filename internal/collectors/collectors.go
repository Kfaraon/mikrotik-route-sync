package collectors

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/netip"
	"strings"
	"time"

	"github.com/Kfaraon/mikrotik-route-sync/internal/classifier"
	"github.com/Kfaraon/mikrotik-route-sync/internal/config"
	"github.com/Kfaraon/mikrotik-route-sync/internal/resolver"
)

type Collector struct {
	cfg      *config.Config
	resolver *resolver.Resolver
	client   *http.Client
}

func New(cfg *config.Config, r *resolver.Resolver) *Collector {
	return &Collector{cfg: cfg, resolver: r, client: &http.Client{Timeout: 30 * time.Second}}
}
func (c *Collector) Collect(ctx context.Context, service string, d classifier.Decision) ([]string, error) {
	switch d.Method {
	case classifier.MethodASN:
		return c.collectASN(ctx, service, d.ASN)
	case classifier.MethodCDN:
		return c.collectCDN(ctx, d)
	case classifier.MethodDynamic:
		return c.collectDynamic(ctx, d)
	case classifier.MethodStaticURL:
		return c.collectStatic(ctx, d.URL)
	case classifier.MethodWHOIS:
		return c.collectWHOIS(ctx, service, d.Domains)
	default:
		return nil, fmt.Errorf("unsupported method %q", d.Method)
	}
}
func (c *Collector) collectASN(ctx context.Context, service string, asn int) ([]string, error) {
	if asn <= 0 {
		return nil, fmt.Errorf("service %s: ASN not detected", service)
	}
	p, err := c.resolver.PrefixesByASN(ctx, asn)
	if err != nil {
		return nil, err
	}
	limit := c.cfg.Overrides[config.NormalizeService(service)].MaxASNPrefixes
	if limit > 0 && len(p) > limit {
		return nil, fmt.Errorf("ASN %d has %d prefixes, limit is %d", asn, len(p), limit)
	}
	return p, nil
}
func (c *Collector) collectWHOIS(ctx context.Context, service string, domains []string) ([]string, error) {
	set := map[string]struct{}{}
	limit := c.cfg.Overrides[config.NormalizeService(service)].MaxASNPrefixes
	for _, d := range domains {
		ips, err := c.resolver.ResolveDomain(ctx, d)
		if err != nil {
			continue
		}
		for _, ip := range ips {
			asn, err := c.resolver.ASNByIP(ctx, ip)
			if err != nil {
				continue
			}
			p, err := c.resolver.PrefixesByASN(ctx, asn)
			if err != nil {
				continue
			}
			if limit > 0 && len(p) > limit {
				return nil, fmt.Errorf("ASN %d has %d prefixes, limit is %d", asn, len(p), limit)
			}
			for _, x := range p {
				set[x] = struct{}{}
			}
		}
	}
	if len(set) == 0 {
		return nil, fmt.Errorf("no prefixes collected for %s", service)
	}
	out := make([]string, 0, len(set))
	for x := range set {
		out = append(out, x)
	}
	return out, nil
}
func (c *Collector) collectDynamic(ctx context.Context, d classifier.Decision) ([]string, error) {
	out, err := c.collectCDN(ctx, classifier.Decision{Provider: "google", Method: classifier.MethodCDN})
	if err != nil {
		out = nil
	}
	for _, domain := range d.Domains {
		ips, e := c.resolver.ResolveDomain(ctx, domain)
		if e != nil {
			continue
		}
		for _, ip := range ips {
			if ip.Is4() {
				out = append(out, netip.PrefixFrom(ip, 32).String())
			} else {
				out = append(out, netip.PrefixFrom(ip, 128).String())
			}
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("dynamic collector returned no prefixes")
	}
	return out, nil
}
func (c *Collector) collectCDN(ctx context.Context, d classifier.Decision) ([]string, error) {
	switch d.Provider {
	case "cloudflare":
		return c.collectLines(ctx, "https://www.cloudflare.com/ips-v4", "https://www.cloudflare.com/ips-v6")
	case "aws":
		return c.collectAWS(ctx)
	case "google":
		return c.collectGoogle(ctx)
	case "fastly":
		return c.collectFastly(ctx)
	case "akamai":
		if d.ASN > 0 {
			return c.resolver.PrefixesByASN(ctx, d.ASN)
		}
		return c.resolver.PrefixesByASN(ctx, 20940)
	default:
		if d.ASN > 0 {
			return c.resolver.PrefixesByASN(ctx, d.ASN)
		}
		return nil, fmt.Errorf("unknown CDN provider")
	}
}
func (c *Collector) collectLines(ctx context.Context, urls ...string) ([]string, error) {
	var out []string
	for _, u := range urls {
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
		resp, err := c.client.Do(req)
		if err != nil {
			return nil, err
		}
		if resp.StatusCode/100 != 2 {
			resp.Body.Close()
			return nil, fmt.Errorf("GET %s: %s", u, resp.Status)
		}
		s := bufio.NewScanner(resp.Body)
		for s.Scan() {
			v := strings.TrimSpace(s.Text())
			if v != "" && !strings.HasPrefix(v, "#") {
				out = append(out, v)
			}
		}
		resp.Body.Close()
		if err := s.Err(); err != nil {
			return nil, err
		}
	}
	return out, nil
}
func (c *Collector) collectStatic(ctx context.Context, u string) ([]string, error) {
	if u == "" {
		return nil, fmt.Errorf("static URL is empty")
	}
	return c.collectLines(ctx, u)
}
func (c *Collector) collectAWS(ctx context.Context) ([]string, error) {
	var p struct {
		Prefixes []struct {
			IPPrefix string `json:"ip_prefix"`
			Service  string `json:"service"`
		} `json:"prefixes"`
		IPv6 []struct {
			IPv6Prefix string `json:"ipv6_prefix"`
			Service    string `json:"service"`
		} `json:"ipv6_prefixes"`
	}
	if err := c.json(ctx, "https://ip-ranges.amazonaws.com/ip-ranges.json", &p); err != nil {
		return nil, err
	}
	var out []string
	for _, x := range p.Prefixes {
		if x.Service == "CLOUDFRONT" {
			out = append(out, x.IPPrefix)
		}
	}
	for _, x := range p.IPv6 {
		if x.Service == "CLOUDFRONT" {
			out = append(out, x.IPv6Prefix)
		}
	}
	return out, nil
}
func (c *Collector) collectGoogle(ctx context.Context) ([]string, error) {
	var p struct {
		Prefixes []struct {
			IPv4Prefix string `json:"ipv4Prefix"`
			IPv6Prefix string `json:"ipv6Prefix"`
			Service    string `json:"service"`
		} `json:"prefixes"`
	}
	if err := c.json(ctx, "https://www.gstatic.com/ipranges/goog.json", &p); err != nil {
		return nil, err
	}
	var out []string
	for _, x := range p.Prefixes {
		if x.IPv4Prefix != "" {
			out = append(out, x.IPv4Prefix)
		}
		if x.IPv6Prefix != "" {
			out = append(out, x.IPv6Prefix)
		}
	}
	return out, nil
}
func (c *Collector) collectFastly(ctx context.Context) ([]string, error) {
	var p struct {
		Addresses []string `json:"addresses"`
		IPv6      []string `json:"ipv6_addresses"`
	}
	if err := c.json(ctx, "https://api.fastly.com/public-ip-list", &p); err != nil {
		return nil, err
	}
	return append(p.Addresses, p.IPv6...), nil
}
func (c *Collector) json(ctx context.Context, u string, dst any) error {
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	resp, err := c.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("GET %s: %s", u, resp.Status)
	}
	return json.NewDecoder(resp.Body).Decode(dst)
}
