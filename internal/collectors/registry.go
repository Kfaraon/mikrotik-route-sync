package collectors

import (
	"context"
	"fmt"
	"time"
)

// Deps — общие зависимости для построения коллекторов.
// Проект работает только с IPv4.
type Deps struct {
	HTTP     *HTTP
	Resolver ResolverAPI
	Cache    PrefixCache
	TTL      time.Duration
}

// Params — параметры конкретного сервиса для построения коллектора.
type Params struct {
	Service     string
	ASN         int
	Domains     []string
	IPs         []string
	StaticURL   string
	MaxPrefixes int
	AlsoCDN     []int
}

// Registry — реестр фабрик коллекторов по имени метода.
type Registry struct {
	deps      *Deps
	factories map[string]func(p *Params) (Collector, error)
}

// NewRegistry создаёт реестр и регистрирует стандартные коллекторы.
func NewRegistry(deps *Deps) *Registry {
	r := &Registry{deps: deps, factories: map[string]func(p *Params) (Collector, error){}}

	r.Register("cdn", func(p *Params) (Collector, error) {
		if p.ASN <= 0 {
			return nil, fmt.Errorf("cdn: ASN is required for service %s", p.Service)
		}
		return NewCDNCollector(p.ASN, r.deps.HTTP), nil
	})

	r.Register("asn", func(p *Params) (Collector, error) {
		if p.ASN <= 0 {
			return nil, fmt.Errorf("asn: ASN is required for service %s", p.Service)
		}
		return NewASNCollector(p.ASN, r.deps.HTTP, r.deps.Cache, r.deps.TTL), nil
	})

	r.Register("whois", func(p *Params) (Collector, error) {
		if len(p.Domains) == 0 && len(p.IPs) == 0 {
			return nil, fmt.Errorf("whois: domains or IPs are required for service %s", p.Service)
		}
		return NewWHOISCollector(r.deps.Resolver, r.deps.HTTP, r.deps.Cache, r.deps.TTL,
			p.Domains, p.IPs, p.MaxPrefixes), nil
	})

	r.Register("static_url", func(p *Params) (Collector, error) {
		if p.StaticURL == "" {
			return nil, fmt.Errorf("static_url: URL is required for service %s", p.Service)
		}
		return NewStaticURLCollector(p.StaticURL, r.deps.HTTP)
	})

	r.Register("dynamic", func(p *Params) (Collector, error) {
		if len(p.Domains) == 0 {
			return nil, fmt.Errorf("dynamic: domains are required for service %s", p.Service)
		}
		return NewDynamicCollector(r.deps.Resolver, r.deps.HTTP, p.Domains, p.AlsoCDN), nil
	})

	return r
}

// Register добавляет фабрику коллектора в реестр.
func (r *Registry) Register(method string, f func(p *Params) (Collector, error)) {
	r.factories[method] = f
}

// Build создаёт коллектор по имени метода и параметрам сервиса.
func (r *Registry) Build(method string, p *Params) (Collector, error) {
	f, ok := r.factories[method]
	if !ok {
		return nil, fmt.Errorf("unknown collector method: %q", method)
	}
	return f(p)
}

// Methods возвращает список зарегистрированных методов.
func (r *Registry) Methods() []string {
	out := make([]string, 0, len(r.factories))
	for m := range r.factories {
		out = append(out, m)
	}
	return out
}

// Build + Collect одной операцией. opts — фильтры из overrides сервиса.
func (r *Registry) Collect(ctx context.Context, method string, p *Params, opts Options) (*Result, error) {
	c, err := r.Build(method, p)
	if err != nil {
		return nil, err
	}
	return c.Collect(ctx, p.Service, opts)
}
