package collectors

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/netip"
	"strings"
)

type BGPToolsCollector struct {
	asn int
}

func NewBGPToolsCollector(asn int) Collector {
	return &BGPToolsCollector{asn: asn}
}

func (c *BGPToolsCollector) Name() string { return "bgptools" }

func (c *BGPToolsCollector) Collect(ctx context.Context, service string, opts Options) (*Result, error) {
	url := fmt.Sprintf("https://bgp.tools/as/%d#prefixes", c.asn)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil { return nil, err }
	req.Header.Set("User-Agent", "mikrotik-route-sync/1.0")

	resp, err := http.DefaultClient.Do(req)
	if err != nil { return nil, fmt.Errorf("bgp.tools request: %w", err) }
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil { return nil, err }

	// Парсинг HTML (упрощённо — ищем CIDR в тексте)
	var prefixes []netip.Prefix
	lines := strings.Split(string(body), "\n")
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if strings.Contains(line, "/") {
			parts := strings.Fields(line)
			for _, part := range parts {
				if p, err := netip.ParsePrefix(part); err == nil {
					prefixes = append(prefixes, p)
				}
			}
		}
	}
	return &Result{Prefixes: prefixes, Source: "bgp.tools", Method: "asn"}, nil
}
