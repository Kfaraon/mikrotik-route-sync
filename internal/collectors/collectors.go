package collectors

import (
	"context"
	"net/netip"
)

// Result — унифицированный результат работы любого сборщика.
type Result struct {
	Prefixes []netip.Prefix
	Source   string // URL / ASN / whois, откуда пришли данные
	Method   string // asn | cdn | dynamic | whois | static_url
}

// Options — параметры, которые может использовать сборщик.
type Options struct {
	Domains        []string
	ASN            int
	URL            string
	MaxASNPrefixes int
	Exclude        []netip.Prefix
}

// Collector — интерфейс любого сборщика.
type Collector interface {
	Name() string
	Collect(ctx context.Context, service string, opts Options) (*Result, error)
}package collectors

import "context"

type Collector interface {
    Collect(ctx context.Context, service string) ([]string, error)
}
