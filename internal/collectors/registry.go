package collectors

import (
	"context"
	"fmt"
)

type Registry struct {
	items map[string]Collector
}

func NewRegistry(apiKeys map[string]string) *Registry {
	r := &Registry{items: map[string]Collector{}}
	r.Register(&ASNCollector{})
	r.Register(&CDNCollector{})
	r.Register(&DynamicCollector{})
	r.Register(&WHOISCollector{})
	r.Register(&StaticURLCollector{})
	
	// Регистрируем Akamai с API ключом (если есть)
	akamaiKey := ""
	if apiKeys != nil {
		akamaiKey = apiKeys["akamai"]
	}
	r.Register(NewAkamaiCollector(akamaiKey))
	
	return r
}

func (r *Registry) Register(c Collector) { r.items[c.Name()] = c }

func (r *Registry) Get(method string) (Collector, error) {
	c, ok := r.items[method]
	if !ok {
		return nil, fmt.Errorf("unknown collector method: %q", method)
	}
	return c, nil
}

func (r *Registry) Collect(ctx context.Context, method, service string, opts Options) (*Result, error) {
	c, err := r.Get(method)
	if err != nil {
		return nil, err
	}
	return c.Collect(ctx, service, opts)
}
