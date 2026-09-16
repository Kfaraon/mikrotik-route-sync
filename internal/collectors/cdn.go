package collectors

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/netip"
	"strings"
	"time"
)

type CDNCollector struct {
	asn int
}

func NewCDNCollector(asn int) Collector {
	return &CDNCollector{asn: asn}
}

func (c *CDNCollector) Name() string { return "cdn" }

// cdnURLs содержит маппинг известных ASN CDN-провайдеров на их публичные списки IP.
// Примечание: Akamai (AS20940) намеренно исключён из этого списка.
// Согласно архитектуре, для Akamai следует использовать метод "asn" (BGPView/RIPEstat).
var cdnURLs = map[int]string{
	13335: "https://www.cloudflare.com/ips-v4",              // Cloudflare
	16509: "https://ip-ranges.amazonaws.com/ip-ranges.json", // AWS CloudFront
	15169: "https://www.gstatic.com/ipranges/goog.json",     // Google
}

func (c *CDNCollector) Collect(ctx context.Context, service string, opts Options) (*Result, error) {
	url, ok := cdnURLs[c.asn]
	if !ok {
		return nil, fmt.Errorf("no static CDN URL for ASN %d (use 'asn' method instead)", c.asn)
	}

	client := &http.Client{Timeout: 30 * time.Second}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}

	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read body: %w", err)
	}

	var prefixes []string

	// Cloudflare возвращает простой текст
	if c.asn == 13335 {
		prefixes = parseTextLines(string(body))
	} else if c.asn == 15169 {
		// Google JSON формат
		var result struct {
			Prefixes []struct {
				IPv4Prefix string `json:"ipv4Prefix"`
				IPv6Prefix string `json:"ipv6Prefix"`
			} `json:"prefixes"`
		}
		if err := json.Unmarshal(body, &result); err == nil {
			for _, p := range result.Prefixes {
				if p.IPv4Prefix != "" {
					prefixes = append(prefixes, p.IPv4Prefix)
				}
				if p.IPv6Prefix != "" {
					prefixes = append(prefixes, p.IPv6Prefix)
				}
			}
		}
	} else {
		// AWS JSON формат
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
			netPrefixes = append(netPrefixes, prefix.Masked())
		}
	}

	return &Result{
		Prefixes: netPrefixes,
		Source:   url,
		Method:   "cdn",
	}, nil
}

func parseTextLines(s string) []string {
	var lines []string
	for _, line := range splitLines(s) {
		line = trimLine(line)
		if line != "" {
			lines = append(lines, line)
		}
	}
	return lines
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

func trimLine(s string) string {
	s = strings.TrimSpace(s)
	if idx := strings.Index(s, "#"); idx >= 0 {
		s = strings.TrimSpace(s[:idx])
	}
	return s
}
