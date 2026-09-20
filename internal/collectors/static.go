package collectors

import (
	"context"
	"fmt"
	"net/url"
)

// StaticURLCollector — произвольные публичные списки CIDR
// (antifilter, re:filter и т.п.): одна сеть на строку, комментарии через #.
type StaticURLCollector struct {
	url  string
	http *HTTP
}

// NewStaticURLCollector проверяет URL (только http/https) и создаёт коллектор.
func NewStaticURLCollector(u string, h *HTTP) (*StaticURLCollector, error) {
	parsed, err := url.Parse(u)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") {
		return nil, fmt.Errorf("static_url: must be a valid http/https URL, got %q", u)
	}
	return &StaticURLCollector{url: u, http: h}, nil
}

func (c *StaticURLCollector) Name() string { return "static_url" }

func (c *StaticURLCollector) Collect(ctx context.Context, service string, opts Options) (*Result, error) {
	lines, err := c.http.FetchLines(ctx, c.url)
	if err != nil {
		return nil, fmt.Errorf("static_url: %w", err)
	}

	out := filterPrefixes(parsePrefixLines(lines), opts)
	if len(out) == 0 {
		return nil, fmt.Errorf("static_url: no valid prefixes in %s", c.url)
	}
	return &Result{
		Prefixes: out,
		Source:   c.url,
		Method:   "static_url",
	}, nil
}
