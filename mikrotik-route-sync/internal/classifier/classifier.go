package classifier

import (
    "context"
    "strings"

    "github.com/example/mikrotik-route-sync/internal/config"
    "github.com/example/mikrotik-route-sync/internal/resolver"
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
    Method  Method
    ASN     int
    Domains []string
    URL     string
}

// knownCDN maps ASN to internal CDN id.
var knownCDN = map[int]string{
    13335: "cloudflare",
    16509: "cloudfront",
    15169: "google",
}

var dynamicServices = map[string]bool{
    "google":  true,
    "youtube": true,
}

type Classifier struct {
    resolver *resolver.Resolver
    cfg      *config.Config
}

func New(cfg *config.Config, r *resolver.Resolver) *Classifier {
    return &Classifier{resolver: r, cfg: cfg}
}

// Classify determines the collection method for a service.
func (c *Classifier) Classify(ctx context.Context, service string) (*Decision, error) {
    name := strings.ToLower(service)

    var domains []string
    if ov, ok := c.cfg.Overrides[service]; ok {
        domains = ov.Domains
    }
    if len(domains) == 0 {
        // try service itself as a domain first, then service.com
        domains = []string{name, name + ".com"}
    }

    var asn int
    for _, d := range domains {
        ips, err := c.resolver.ResolveDomain(ctx, d)
        if err != nil || len(ips) == 0 {
            continue
        }
        a, err := c.resolver.ASNByIP(ctx, ips[0])
        if err == nil {
            asn = a
            break
        }
    }

    // Priority classification
    if dynamicServices[name] {
        return &Decision{Method: MethodDynamic, ASN: asn, Domains: domains}, nil
    }
    if _, ok := knownCDN[asn]; ok {
        return &Decision{Method: MethodCDN, ASN: asn}, nil
    }
    if asn != 0 {
        return &Decision{Method: MethodASN, ASN: asn, Domains: domains}, nil
    }
    return &Decision{Method: MethodWHOIS, Domains: domains}, nil
}