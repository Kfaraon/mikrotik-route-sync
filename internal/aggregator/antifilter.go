package collectors

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/netip"
	"strings"
)

type AntifilterCollector struct {
	url string
}

func NewAntifilterCollector() Collector {
	return &AntifilterCollector{url: "https://antifilter.download/list/allyouneed.lst"}
}

func NewRefilterCollector() Collector {
	return &AntifilterCollector{url: "https://refilter.online/downloads/list.txt"}
}

func (c *AntifilterCollector) Name() string { return "static_url" }

func (c *AntifilterCollector) Collect(ctx context.Context, service string, opts Options) (*Result, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.url, nil)
	if err != nil { return nil, err }

	resp, err := http.DefaultClient.Do(req)
	if err != nil { return nil, fmt.Errorf("antifilter request: %w", err) }
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil { return nil, err }

	var prefixes []netip.Prefix
	for _, line := range strings.Split(string(body), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") { continue }
		if p, err := netip.ParsePrefix(line); err == nil {
			prefixes = append(prefixes, p)
		}
	}
	return &Result{Prefixes: prefixes, Source: c.url, Method: "static_url"}, nil
}
