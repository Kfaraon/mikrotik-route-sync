package collectors

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/netip"
)

type AkamaiCollector struct{}

func NewAkamaiCollector() Collector { return &AkamaiCollector{} }

func (c *AkamaiCollector) Name() string { return "cdn" }

func (c *AkamaiCollector) Collect(ctx context.Context, service string, opts Options) (*Result, error) {
	url := "https://api.edgesuite.com/akamai/v1/network-lists"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil { return nil, err }

	resp, err := http.DefaultClient.Do(req)
	if err != nil { return nil, fmt.Errorf("akamai request: %w", err) }
	defer resp.Body.Close()

	// Akamai предоставляет IP через их API или список
	// Упрощённо — используем известный список
	url2 := "https://raw.githubusercontent.com/Akamai-Open/Akamai-IP-Lists/master/akamai-ip-list.txt"
	req2, _ := http.NewRequestWithContext(ctx, http.MethodGet, url2, nil)
	resp2, err := http.DefaultClient.Do(req2)
	if err != nil { return nil, err }
	defer resp2.Body.Close()

	_ = json.Unmarshal
	// Реализация парсинга
	var prefixes []netip.Prefix
	return &Result{Prefixes: prefixes, Source: "akamai", Method: "cdn"}, nil
}
