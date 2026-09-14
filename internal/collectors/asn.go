package collectors

import (
    "context"

    "github.com/example/mikrotik-route-sync/internal/resolver"
)

type ASNCollector struct {
    resolver *resolver.Resolver
    asn      int
}

func NewASNCollector(r *resolver.Resolver, asn int) *ASNCollector {
    return &ASNCollector{resolver: r, asn: asn}
}

func (c *ASNCollector) Collect(ctx context.Context, _ string) ([]string, error) {
    return c.resolver.PrefixesByASN(ctx, c.asn)
}