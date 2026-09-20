package collectors

import (
	"context"
	"fmt"
	"strings"
)

// CDN URL-источники по ASN (PROMPT II.2). Только IPv4.
// Примечание: Akamai (AS20940) намеренно исключён — для него используется метод "asn".
var cdnSources = map[int]cdnSource{
	13335: { // Cloudflare
		url:  "https://www.cloudflare.com/ips-v4",
		kind: "text",
	},
	16509: { // AWS CloudFront
		url:  "https://ip-ranges.amazonaws.com/ip-ranges.json",
		kind: "aws",
	},
	15169: { // Google
		url:  "https://www.gstatic.com/ipranges/goog.json",
		kind: "google",
	},
	54825: { // Fastly
		url:  "https://api.fastly.com/public-ip-list",
		kind: "fastly",
	},
}

type cdnSource struct {
	url  string
	kind string
}

// CDNCollector — официальные публичные списки IPv4-диапазонов CDN-провайдеров.
type CDNCollector struct {
	asn  int
	http *HTTP
}

// NewCDNCollector создаёт CDN-коллектор для ASN.
func NewCDNCollector(asn int, h *HTTP) *CDNCollector {
	return &CDNCollector{asn: asn, http: h}
}

func (c *CDNCollector) Name() string { return "cdn" }

func (c *CDNCollector) Collect(ctx context.Context, service string, opts Options) (*Result, error) {
	src, ok := cdnSources[c.asn]
	if !ok {
		return nil, fmt.Errorf("no CDN source for ASN %d (use 'asn' method instead)", c.asn)
	}

	lines, err := c.fetchSource(ctx, src.kind, src.url)
	if err != nil {
		return nil, err
	}

	out := filterPrefixes(parsePrefixLines(lines), opts)
	if len(out) == 0 {
		return nil, fmt.Errorf("cdn: no prefixes fetched for AS%d", c.asn)
	}
	return &Result{
		Prefixes: out,
		Source:   src.url,
		Method:   "cdn",
	}, nil
}

// fetchSource парсит конкретный формат CDN-ответа, возвращает строки CIDR.
func (c *CDNCollector) fetchSource(ctx context.Context, kind, u string) ([]string, error) {
	switch kind {
	case "text":
		return c.http.FetchLines(ctx, u)

	case "google":
		var data struct {
			Prefixes []struct {
				IPv4Prefix string `json:"ipv4Prefix"`
			} `json:"prefixes"`
		}
		if err := c.http.FetchJSON(ctx, u, &data); err != nil {
			return nil, err
		}
		var out []string
		for _, p := range data.Prefixes {
			if p.IPv4Prefix != "" {
				out = append(out, p.IPv4Prefix)
			}
		}
		return out, nil

	case "aws":
		// Фильтр только по service=CLOUDFRONT* (PROMPT II.2).
		var data struct {
			Prefixes []struct {
				IPPrefix string `json:"ip_prefix"`
				Service  string `json:"service"`
				Region   string `json:"region"`
			} `json:"prefixes"`
		}
		if err := c.http.FetchJSON(ctx, u, &data); err != nil {
			return nil, err
		}
		var out []string
		for _, p := range data.Prefixes {
			if !strings.HasPrefix(strings.ToUpper(p.Service), "CLOUDFRONT") {
				continue
			}
			if p.IPPrefix != "" {
				out = append(out, p.IPPrefix)
			}
		}
		return out, nil

	case "fastly":
		var data struct {
			Prefixes []string `json:"prefixes"`
			IPv4     []string `json:"addresses"`
		}
		if err := c.http.FetchJSON(ctx, u, &data); err != nil {
			return nil, err
		}
		if len(data.Prefixes) > 0 {
			return data.Prefixes, nil
		}
		return data.IPv4, nil
	}

	return nil, fmt.Errorf("cdn: unknown source kind %q", kind)
}
