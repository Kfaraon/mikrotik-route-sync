package collectors

import "context"

type Collector interface {
    Collect(ctx context.Context, service string) ([]string, error)
}