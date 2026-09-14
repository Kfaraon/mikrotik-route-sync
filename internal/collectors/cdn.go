package collectors

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/netip"
	"time"
)

type CDNCollector struct {
	asn int
}

func NewCDNCollector(asn int) Collector {
	return &CDNCollector{asn: asn}
}

func (c *CDNCollector) Name() string { return "cdn" }

func (c *CDNCollector) Collect(ctx context.Context, service string, opts Options) (*Result, error) {
	var url string
	switch c.asn {
	case 13335:
		url = "https://www.cloudflare.com/ips-v4"
	case 16509:
		url = "https://ip-ranges.amazonaws.com/ip-ranges.json"
	case 15169:
		url = "https://www.gstatic.com/ipranges/goog.json"
	default:
		return nil, fmt.Errorf("unsupported CDN ASN: %d", c.asn)
	}

	client := &http.Client{Timeout: 30 * time.Second}
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return nil, err
	}

	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	var prefixes []netip.Prefix

	if c.asn == 13335 {
		// Cloudflare: plain text list
		lines := splitLines(string(body))
		for _, line := range lines {
			if p, err := netip.ParsePrefix(line); err == nil {
				prefixes = append(prefixes, p.Masked())
			}
		}
	} else if c.asn == 16509 {
		// AWS CloudFront: JSON
		var data struct {
			Prefixes []struct {
				IPPrefix string `json:"ip_prefix"`
				Service  string `json:"service"`
			} `json:"prefixes"`
		}
		if err := json.Unmarshal(body, &data); err != nil {
			return nil, err
		}
		for _, p := range data.Prefixes {
			if p.Service == "CLOUDFRONT" {
				if prefix, err := netip.ParsePrefix(p.IPPrefix); err == nil {
					prefixes = append(prefixes, prefix.Masked())
				}
			}
		}
	} else if c.asn == 15169 {
		// Google: JSON
		var data struct {
			Prefixes []struct {
				IPv4Prefix string `json:"ipv4Prefix"`
			} `json:"prefixes"`
		}
		if err := json.Unmarshal(body, &data); err != nil {
			return nil, err
		}
		for _, p := range data.Prefixes {
			if p.IPv4Prefix != "" {
				if prefix, err := netip.ParsePrefix(p.IPv4Prefix); err == nil {
					prefixes = append(prefixes, prefix.Masked())
				}
			}
		}
	}

	return &Result{
		Prefixes: prefixes,
		Source:   url,
		Method:   "cdn",
	}, nil
}

func splitLines(s string) []string {
	var lines []string
	start := 0
	for i := 0; i < len(s); i++ {
		if s[i] == '\n' {
			line := s[start:i]
			if len(line) > 0 && line[len(line)-1] == '\r' {
				line = line[:len(line)-1]
			}
			if line != "" {
				lines = append(lines, line)
			}
			start = i + 1
		}
	}
	if start < len(s) {
		lines = append(lines, s[start:])
	}
	return lines
}
