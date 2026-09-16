package collectors

import (
	"context"
	"fmt"
)

// Registry управляет доступными коллекторами IP-диапазонов.
type Registry struct {
	items map[string]Collector
}

// NewRegistry создаёт новый реестр и регистрирует стандартные коллекторы.
// Параметр apiKeys зарезервирован для будущего расширения (например, платные API).
func NewRegistry(apiKeys map[string]string) *Registry {
	r := &Registry{items: map[string]Collector{}}
	
	// Регистрация стандартных коллекторов
	r.Register(&ASNCollector{})
	r.Register(&CDNCollector{})
	r.Register(&DynamicCollector{})
	r.Register(&WHOISCollector{})
	r.Register(&StaticURLCollector{})
	
	// Примечание: Akamai (AS20940) обрабатывается через ASNCollector (BGPView/RIPEstat),
	// поэтому отдельный AkamaiCollector не регистрируется.
	
	return r
}

// Register добавляет коллектор в реестр.
func (r *Registry) Register(c Collector) { 
	r.items[c.Name()] = c 
}

// Get возвращает коллектор по имени метода.
func (r *Registry) Get(method string) (Collector, error) {
	c, ok := r.items[method]
	if !ok {
		return nil, fmt.Errorf("unknown collector method: %q", method)
	}
	return c, nil
}

// Collect делегирует сбор данных соответствующему коллектору.
func (r *Registry) Collect(ctx context.Context, method, service string, opts Options) (*Result, error) {
	c, err := r.Get(method)
	if err != nil {
		return nil, err
	}
	return c.Collect(ctx, service, opts)
}
