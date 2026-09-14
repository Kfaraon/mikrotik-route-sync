package collectors

import (
    "bufio"
    "context"
    "encoding/json"
    "fmt"
    "net/http"
    "strings"
    "time"
)

type CDNCollector struct {
    asn  int
    http *http.Client
}

func NewCDNCollector(asn int) *CDNCollector {
    return &CDNCollector{asn: asn, http: &http.Client{Timeout: 30 * time.Second}}
}

func (c *CDNCollector) Collect(ctx context.Context, _ string) ([]string, error) {
    switch c.asn {
    case 13335:
        return c.fetchCloudflare(ctx)
    case 15169:
        return c.fetchGoogle(ctx)
    case 16509:
        return c.fetchAWSCloudFront(ctx)
    }
    return nil, fmt.Errorf("unknown CDN ASN %d", c.asn)
}

func (c *CDNCollector) fetchCloudflare(ctx context.Context) ([]string, error) {
    return c.fetchLines(ctx, "https://www.cloudflare.com/ips-v4")
}

func (c *CDNCollector) fetchGoogle(ctx context.Context) ([]string, error) {
    req, _ := http.NewRequestWithContext(ctx, http.MethodGet, "https://www.gstatic.com/ipranges/goog.json", nil)
    resp, err := c.http.Do(req)
    if err != nil {
        return nil, err
    }
    defer resp.Body.Close()
    var data struct {
        Prefixes []struct {
            IPv4Prefix string `json:"ipv4Prefix"`
        } `json:"prefixes"`
    }
    if err := json.NewDecoder(resp.Body).Decode(&data); err != nil {
        return nil, err
    }
    out := make([]string, 0, len(data.Prefixes))
    for _, p := range data.Prefixes {
        if p.IPv4Prefix != "" {
            out = append(out, p.IPv4Prefix)
        }
    }
    return out, nil
}

func (c *CDNCollector) fetchAWSCloudFront(ctx context.Context) ([]string, error) {
    req, _ := http.NewRequestWithContext(ctx, http.MethodGet, "https://ip-ranges.amazonaws.com/ip-ranges.json", nil)
    resp, err := c.http.Do(req)
    if err != nil {
        return nil, err
    }
    defer resp.Body.Close()
    var data struct {
        Prefixes []struct {
            Service string `json:"service"`
            IPv4    string `json:"ip_prefix"`
        } `json:"prefixes"`
    }
    if err := json.NewDecoder(resp.Body).Decode(&data); err != nil {
        return nil, err
    }
    var out []string
    for _, p := range data.Prefixes {
        if p.Service == "CLOUDFRONT" {
            out = append(out, p.IPv4)
        }
    }
    return out, nil
}

func (c *CDNCollector) fetchLines(ctx context.Context, url string) ([]string, error) {
    req, _ := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
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