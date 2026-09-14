package classifier

import (
	"context"
	"fmt"
	"net/netip"
	"strconv"
	"strings"

	"github.com/Kfaraon/mikrotik-route-sync/internal/config"
	"github.com/Kfaraon/mikrotik-route-sync/internal/resolver"
)

type Method string

const (
	MethodASN       Method = "asn"
	MethodCDN       Method = "cdn"
	MethodDynamic   Method = "dynamic"
	MethodWHOIS     Method = "whois"
	MethodStaticURL Method = "static_url"
)

type Decision struct {
	Method   Method   `json:"method"`
	ASN      int      `json:"asn,omitempty"`
	Domains  []string `json:"domains,omitempty"`
	URL      string   `json:"url,omitempty"`
	Provider string   `json:"provider,omitempty"`
}

var knownASN = map[int]string{13335: "cloudflare", 16509: "aws", 15169: "google", 20940: "akamai", 54113: "fastly"}
var names = map[string]struct {
	method   Method
	provider string
}{"cloudflare": {MethodCDN, "cloudflare"}, "youtube": {MethodDynamic, "google"}, "google": {MethodDynamic, "google"}, "cloudfront": {MethodCDN, "aws"}, "aws": {MethodCDN, "aws"}, "fastly": {MethodCDN, "fastly"}, "akamai": {MethodCDN, "akamai"}}

type Classifier struct {
	cfg      *config.Config
	resolver *resolver.Resolver
}

func New(cfg *config.Config, r *resolver.Resolver) *Classifier {
	return &Classifier{cfg: cfg, resolver: r}
}
func (c *Classifier) Classify(ctx context.Context, service string) (Decision, error) {
	name := config.NormalizeService(service)
	ov := c.cfg.Overrides[name]
	if ov.Method != "" {
		d := Decision{Method: Method(strings.ToLower(ov.Method)), ASN: ov.ASN, Domains: ov.Domains, URL: ov.StaticURL}
		if d.Method == MethodStaticURL && d.URL == "" {
			return d, fmt.Errorf("static_url override for %s has no URL", name)
		}
		return d, nil
	}
	if ov.StaticURL != "" {
		return Decision{Method: MethodStaticURL, URL: ov.StaticURL, Domains: ov.Domains}, nil
	}
	if strings.HasPrefix(strings.ToUpper(name), "AS") {
		n, err := strconv.Atoi(strings.TrimPrefix(strings.ToUpper(name), "AS"))
		if err == nil && n > 0 {
			return Decision{Method: MethodASN, ASN: n}, nil
		}
	}
	if n, ok := names[name]; ok {
		return Decision{Method: n.method, Provider: n.provider, ASN: ov.ASN, Domains: chooseDomains(name, ov.Domains)}, nil
	}
	if ip, err := netip.ParseAddr(name); err == nil {
		asn, e := c.resolver.ASNByIP(ctx, ip.Unmap())
		if e != nil {
			return Decision{Method: MethodWHOIS, Domains: []string{name}}, nil
		}
		return Decision{Method: MethodASN, ASN: asn, Domains: []string{name}}, nil
	}
	domains := chooseDomains(name, ov.Domains)
	for _, d := range domains {
		ips, err := c.resolver.ResolveDomain(ctx, d)
		if err != nil {
			continue
		}
		for _, ip := range ips {
			asn, err := c.resolver.ASNByIP(ctx, ip)
			if err != nil || asn == 0 {
				continue
			}
			if provider, ok := knownASN[asn]; ok {
				m := MethodCDN
				if provider == "google" {
					m = MethodDynamic
				}
				return Decision{Method: m, ASN: asn, Domains: domains, Provider: provider}, nil
			}
			return Decision{Method: MethodASN, ASN: asn, Domains: domains}, nil
		}
	}
	return Decision{Method: MethodWHOIS, Domains: domains}, nil
}
func chooseDomains(name string, override []string) []string {
	if len(override) > 0 {
		return append([]string(nil), override...)
	}
	if strings.Contains(name, ".") {
		return []string{name}
	}
	return []string{name + ".com", name}
}
