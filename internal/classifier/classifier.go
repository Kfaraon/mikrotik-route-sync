package classifier

import (
	"context"
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
	MethodRIPEstat  Method = "ripestat"
	MethodBGPTools  Method = "bgptools"
)

type Decision struct {
	Method  Method
	ASN     int
	Domains []string
	URL     string
}

var knownCDN = map[int]string{
	13335: "cloudflare",
	16509: "cloudfront",
	15169: "google",
	20940: "akamai",
	54113: "fastly",
}

var dynamicServices = map[string]bool{
	"google":  true,
	"youtube": true,
}

var staticURLServices = map[string]string{
	"antifilter": "https://antifilter.download/list/allyouneed.lst",
	"refilter":   "https://refilter.online/downloads/list.txt",
}

type Classifier struct {
	resolver *resolver.Resolver
	cfg      *config.Config
}

func New(cfg *config.Config, r *resolver.Resolver) *Classifier {
	return &Classifier{resolver: r, cfg: cfg}
}

func (c *Classifier) Classify(ctx context.Context, service string) (*Decision, error) {
	name := strings.ToLower(service)

	// Приоритет 1: статические списки
	if url, ok := staticURLServices[name]; ok {
		return &Decision{Method: MethodStaticURL, URL: url}, nil
	}

	// Приоритет 2: динамические сервисы
	if dynamicServices[name] {
		domains := []string{name + ".com"}
		return &Decision{Method: MethodDynamic, Domains: domains}, nil
	}

	// Определение доменов
	var domains []string
	if ov, ok := c.cfg.Overrides[service]; ok && len(ov.Domains) > 0 {
		domains = ov.Domains
	} else {
		domains = []string{name, name + ".com"}
	}

	// Определение ASN
	var asn int
	for _, d := range domains {
		ips, err := c.resolver.ResolveDomain(ctx, d)
		if err != nil || len(ips) == 0 { continue }
		a, err := c.resolver.GetASN(ctx, ips[0])
		if err == nil && a > 0 { asn = a; break }
	}

	// Приоритет 3: известные CDN
	if _, ok := knownCDN[asn]; ok {
		return &Decision{Method: MethodCDN, ASN: asn}, nil
	}

	// Приоритет 4: крупные ASN (используем RIPEstat как запасной)
	if asn > 0 {
		return &Decision{Method: MethodASN, ASN: asn, Domains: domains}, nil
	}

	// Приоритет 5: WHOIS fallback
	return &Decision{Method: MethodWHOIS, Domains: domains}, nil
}
