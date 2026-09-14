package collectors

import (
	"context"
	"encoding/json"
	"fmt"
	"net/netip"
	"strings"
)

type cdnSource struct {
	URL   string
	Parse func([]byte) ([]netip.Prefix, error)
}

// Ключ — имя сервиса, как его вводит пользователь (или классификатор).
var cdnSources = map[string]cdnSource{
	"cloudflare": {
		URL:   "https://www.cloudflare.com/ips-v4",
		Parse: parsePlainLines,
	},
	"cloudfront": {
		URL:   "https://ip-ranges.amazonaws.com/ip-ranges.json",
		Parse: parseAWSRanges("CLOUDFRONT"),
	},
	"aws": {
		URL:   "https://ip-ranges.amazonaws.com/ip-ranges.json",
		Parse: parseAWSRanges(""),
	},
	"google": {
		URL:   "https://www.gstatic.com/ipranges/goog.json",
		Parse: parseGoogleRanges,
	},
	"fastly": {
		URL:   "https://api.fastly.com/public-ip-list",
		Parse: parseFastlyRanges,
	},
	// Akamai не отдаёт стабильный публичный JSON.
	// Для akamai используем метод asn (см. classifier).
}

type CDNCollector struct{}

func (c *CDNCollector) Name() string { return "cdn" }

func (c *CDNCollector) Collect(ctx context.Context, service string, opts Options) (*Result, error) {
	src, ok := cdnSources[strings.ToLower(service)]
	if !ok {
		return nil, fmt.Errorf("no CDN source for %q", service)
	}
	data, err := fetch(ctx, src.URL)
	if err != nil {
		return nil, err
	}
	prefixes, err := src.Parse(data)
	if err != nil {
		return nil, err
	}
	return &Result{Prefixes: prefixes, Source: src.URL, Method: "cdn"}, nil
}

func parsePlainLines(data []byte) ([]netip.Prefix, error) {
	var out []netip.Prefix
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if p, err := netip.ParsePrefix(line); err == nil {
			out = append(out, p.Masked())
		}
	}
	return out, nil
}

func parseGoogleRanges(data []byte) ([]netip.Prefix, error) {
	var v struct {
		Prefixes []struct {
			IPv4 string `json:"ipv4Prefix"`
			IPv6 string `json:"ipv6Prefix"`
		} `json:"prefixes"`
	}
	if err := json.Unmarshal(data, &v); err != nil {
		return nil, err
	}
	var out []netip.Prefix
	for _, p := range v.Prefixes {
		if p.IPv4 == "" {
			continue
		}
		if pr, err := netip.ParsePrefix(p.IPv4); err == nil {
			out = append(out, pr.Masked())
		}
	}
	return out, nil
}

func parseAWSRanges(serviceFilter string) func([]byte) ([]netip.Prefix, error) {
	return func(data []byte) ([]netip.Prefix, error) {
		var v struct {
			Prefixes []struct {
				Service string `json:"service"`
				IPv4    string `json:"ip_prefix"`
			} `json:"prefixes"`
		}
		if err := json.Unmarshal(data, &v); err != nil {
			return nil, err
		}
		var out []netip.Prefix
		for _, p := range v.Prefixes {
			if serviceFilter != "" && p.Service != serviceFilter {
				continue
			}
			if pr, err := netip.ParsePrefix(p.IPv4); err == nil {
				out = append(out, pr.Masked())
			}
		}
		return out, nil
	}
}

func parseFastlyRanges(data []byte) ([]netip.Prefix, error) {
	var v struct {
		Addresses []string `json:"addresses"`
	}
	if err := json.Unmarshal(data, &v); err != nil {
		return nil, err
	}
	var out []netip.Prefix
	for _, a := range v.Addresses {
		if p, err := netip.ParsePrefix(a); err == nil {
			out = append(out, p.Masked())
		}
	}
	return out, nil
}
