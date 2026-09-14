package collectors

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/netip"
)

type CDNCollector struct {
	asn int
}

func NewCDNCollector(asn int) Collector {
	return &CDNCollector{asn: asn}
}

func (c *CDNCollector) Name() string { return "cdn" }

var cdnURLs = map[int]string{
	13335: "https://www.cloudflare.com/ips-v4",
	16509: "https://ip-ranges.amazonaws.com/ip-ranges.json",
	15169: "https://www.gstatic.com/ipranges/goog.json",
}

func (c *CDNCollector) Collect(ctx context.Context, service string, opts Options) (*Result, error) {
	url, ok := cdnURLs[c.asn]
	if !ok {
		return nil, fmt.Errorf("no CDN URL for ASN %d", c.asn)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	var prefixes []string

	// Cloudflare возвращает простой текст
	if c.asn == 13335 {
		for _, line := range splitLines(string(body)) {
			if line != "" {
				prefixes = append(prefixes, line)
			}
		}
	} else {
		// JSON формат для AWS и Google
		var result struct {
			Prefixes []struct {
				IPPrefix string `json:"ip_prefix"`
			} `json:"prefixes"`
		}
		if err := json.Unmarshal(body, &result); err == nil {
			for _, p := range result.Prefixes {
				prefixes = append(prefixes, p.IPPrefix)
			}
		}
	}

	var netPrefixes []netip.Prefix
	for _, p := range prefixes {
		if prefix, err := netip.ParsePrefix(p); err == nil {
			netPrefixes = append(netPrefixes, prefix)
		}
	}

	return &Result{
		Prefixes: netPrefixes,
		Source:   url,
		Method:   "cdn",
	}, nil
}

func splitLines(s string) []string {
	var lines []string
	start := 0
	for i := 0; i < len(s); i++ {
		if s[i] == '\n' {
			lines = append(lines, s[start:i])
			start = i + 1
		}
	}
	if start < len(s) {
		lines = append(lines, s[start:])
	}
	return lines
}
