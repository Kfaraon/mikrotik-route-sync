package collectors

import (
	"context"
	"net/netip"
)

type Result struct {
	Prefixes []netip.Prefix
	Source   string
	Method   string
}

type Options struct {
	Domains        []string
	ASN            int
	URL            string
	MaxASNPrefixes int
	Exclude        []netip.Prefix
}

type Collector interface {
	Name() string
	Collect(ctx context.Context, service string, opts Options) (*Result, error)
}
