package collectors

import (
	"context"
	"fmt"
	"net/netip"
)

// StaticURLCollector — загрузка произвольного списка CIDR по URL.
type StaticURLCollector struct{}

func (s *StaticURLCollector) Name() string { return "static_url" }

func (s *StaticURLCollector) Collect(ctx context.Context, service string, opts Options) (*Result, error) {
	if opts.URL == "" {
		return nil, fmt.Errorf("static_url collector requires URL")
	}
	data, err := fetch(ctx, opts.URL)
	if err != nil {
		return nil, err
	}
	prefixes, err := parsePlainLines(data)
	if err != nil {
		return nil, err
	}
	var out []netip.Prefix
	seen := map[netip.Prefix]struct{}{}
	for _, p := range prefixes {
		if _, ok := seen[p]; ok {
			continue
		}
		seen[p] = struct{}{}
		out = append(out, p)
	}
	return &Result{Prefixes: out, Source: opts.URL, Method: "static_url"}, nil
}package collectors

import (
    "bufio"
    "context"
    "net/http"
    "strings"
    "time"
)

type StaticURLCollector struct {
    url  string
    http *http.Client
}

func NewStaticURLCollector(url string) *StaticURLCollector {
    return &StaticURLCollector{url: url, http: &http.Client{Timeout: 30 * time.Second}}
}

func (c *StaticURLCollector) Collect(ctx context.Context, _ string) ([]string, error) {
    req, _ := http.NewRequestWithContext(ctx, http.MethodGet, c.url, nil)
    resp, err := c.http.Do(req)
    if err != nil {
        return nil, err
    }
    defer resp.Body.Close()
    var out []string
    sc := bufio.NewScanner(resp.Body)
    for sc.Scan() {
        line := strings.TrimSpace(sc.Text())
        if line != "" && !strings.HasPrefix(line, "#") {
            out = append(out, line)
        }
    }
    return out, sc.Err()
}
