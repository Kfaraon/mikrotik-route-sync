package classifier

import (
	"context"
	"fmt"
	"strings"

	"github.com/Kfaraon/mikrotik-route-sync/internal/config"
	"github.com/Kfaraon/mikrotik-route-sync/internal/resolver"
)

type Method string

const (
	MethodASN     Method = "asn"
	MethodCDN     Method = "cdn"
	MethodDynamic Method = "dynamic"
	MethodWHOIS   Method = "whois"
	MethodStatic  Method = "static_url"
)

type Decision struct {
	Method  Method
	ASN     int
	Domains []string
	URL     string
}

type Classifier struct {
	cfg      *config.Config
	resolver *resolver.Resolver
}

func New(cfg *config.Config, r *resolver.Resolver) *Classifier {
	return &Classifier{cfg: cfg, resolver: r}
}

// Известные CDN по ASN
var cdnASNs = map[int]string{
	13335: "cloudflare",
	16509: "aws",
	15169: "google",
	20940: "akamai",
	54113: "fastly",
}

// Сервисы с динамическим резолвингом
var dynamicServices = map[string]bool{
	"google":   true,
	"youtube":  true,
	"facebook": true,
	"instagram": true,
}

func (c *Classifier) Classify(ctx context.Context, service string) (*Decision, error) {
	service = strings.ToLower(strings.TrimSpace(service))

	// Проверяем переопределения в конфиге
	if ov, ok := c.cfg.Overrides[service]; ok && len(ov.Domains) > 0 {
		return c.classifyByDomains(ctx, service, ov.Domains)
	}

	// Динамические сервисы
	if dynamicServices[service] {
		return &Decision{
			Method:  MethodDynamic,
			Domains: []string{service + ".com"},
		}, nil
	}

	// Пробуем резолвить как домен
	domains := []string{service + ".com", service + ".org", service + ".net"}
	
	for _, domain := range domains {
		ips, err := c.resolver.ResolveDomain(ctx, domain)
		if err != nil || len(ips) == 0 {
			continue
		}

		asn, err := c.resolver.GetASN(ctx, ips[0])
		if err != nil || asn == 0 {
			continue
		}

		// Проверяем, является ли это CDN
		if cdnName, ok := cdnASNs[asn]; ok {
			return &Decision{
				Method:  MethodCDN,
				ASN:     asn,
				Domains: []string{domain},
			}, fmt.Errorf("detected CDN: %s", cdnName)
		}

		// Обычный сервис с ASN
		return &Decision{
			Method:  MethodASN,
			ASN:     asn,
			Domains: []string{domain},
		}, nil
	}

	// Fallback: WHOIS метод
	return &Decision{
		Method:  MethodWHOIS,
		Domains: domains,
	}, nil
}

func (c *Classifier) classifyByDomains(ctx context.Context, service string, domains []string) (*Decision, error) {
	if len(domains) == 0 {
		return nil, fmt.Errorf("no domains for service %s", service)
	}

	ips, err := c.resolver.ResolveDomain(ctx, domains[0])
	if err != nil || len(ips) == 0 {
		return nil, fmt.Errorf("cannot resolve %s: %w", domains[0], err)
	}

	asn, err := c.resolver.GetASN(ctx, ips[0])
	if err != nil || asn == 0 {
		return &Decision{
			Method:  MethodWHOIS,
			Domains: domains,
		}, nil
	}

	if _, ok := cdnASNs[asn]; ok {
		return &Decision{
			Method:  MethodCDN,
			ASN:     asn,
			Domains: domains,
		}, nil
	}

	return &Decision{
		Method:  MethodASN,
		ASN:     asn,
		Domains: domains,
	}, nil
}
