package collectors

import (
	"github.com/Kfaraon/mikrotik-route-sync/internal/resolver"
)

// Registry управляет зарегистрированными сборщиками
type Registry struct {
	resolver *resolver.Resolver
}

func NewRegistry(r *resolver.Resolver) *Registry {
	return &Registry{resolver: r}
}

// GetCollector возвращает подходящий collector для метода
func (reg *Registry) GetCollector(method string, asn int, domains []string, opts Options) (Collector, error) {
	switch method {
	case "asn":
		return NewASNCollector(reg.resolver, asn), nil
	case "cdn":
		// Для известных CDN используем CDNCollector, для Akamai - AkamaiCollector
		if asn == 20940 {
			return NewAkamaiCollector(), nil
		}
		return NewCDNCollector(asn), nil
	case "dynamic":
		return NewDynamicCollector(reg.resolver, domains), nil
	case "whois":
		return NewWHOISCollector(reg.resolver, domains, opts.MaxASNPrefixes), nil
	case "static_url":
		return NewStaticCollector(opts.StaticURL), nil
	default:
		return nil, fmt.Errorf("unknown method: %s", method)
	}
}
