package collectors

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