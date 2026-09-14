package collectors

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/netip"
)

type RIPEstatCollector struct {
	asn int
}

func NewRIPEstatCollector(asn int) Collector {
	return &RIPEstatCollector{asn: asn}
}

func (c *RIPEstatCollector) Name() string { return "ripestat" }

func (c *RIPEstatCollector) Collect(ctx context.Context, service string, opts Options) (*Result, error) {
	url := fmt.Sprintf("https://stat.ripe.net/data/announced-prefixes/data.json?resource=AS%d", c.asn)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil { return nil, err }

	resp, err := http.DefaultClient.Do(req)
	if err != nil { return nil, fmt.Errorf("ripestat request: %w", err) }
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("ripestat returned %d", resp.StatusCode)
	}

	var result struct {
		Data struct {
			Prefixes []struct {
				Prefix string `json:"prefix"`
			} `json:"prefixes"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("decode ripestat: %w", err)
	}

	var prefixes []netip.Prefix
	for _, p := range result.Data.Prefixes {
		if prefix, err := netip.ParsePrefix(p.Prefix); err == nil {
			prefixes = append(prefixes, prefix)
		}
	}
	return &Result{Prefixes: prefixes, Source: "ripestat", Method: "asn"}, nil
}
