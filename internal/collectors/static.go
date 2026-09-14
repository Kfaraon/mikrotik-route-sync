package collectors

import (
	"bufio"
	"context"
	"io"
	"net/http"
	"net/netip"
	"strings"
	"time"
)

type StaticCollector struct {
	url string
}

func NewStaticCollector(url string) Collector {
	return &StaticCollector{url: url}
}

func (c *StaticCollector) Name() string { return "static_url" }

func (c *StaticCollector) Collect(ctx context.Context, service string, opts Options) (*Result, error) {
	client := &http.Client{Timeout: 30 * time.Second}
	req, err := http.NewRequestWithContext(ctx, "GET", c.url, nil)
	if err != nil {
		return nil, err
	}

	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var prefixes []netip.Prefix
	excludeSet := make(map[netip.Prefix]bool)
	for _, ex := range opts.Exclude {
		excludeSet[ex.Masked()] = true
	}

	scanner := bufio.NewScanner(resp.Body)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if prefix, err := netip.ParsePrefix(line); err == nil {
			prefix = prefix.Masked()
			if !excludeSet[prefix] {
				prefixes = append(prefixes, prefix)
			}
		}
	}

	return &Result{
		Prefixes: prefixes,
		Source:   c.url,
		Method:   "static_url",
	}, nil
}
