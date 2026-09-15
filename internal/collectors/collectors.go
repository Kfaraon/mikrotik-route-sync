package collectors

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

// HTTP обёртка над http.Client с лимитами.
type HTTP struct {
	Client    *http.Client
	MaxBytes  int64
}

// NewHTTP создаёт HTTP-клиент с указанными таймаутами.
func NewHTTP(client *http.Client, maxMB int) *HTTP {
	if client == nil {
		client = http.DefaultClient
	}
	if maxMB <= 0 {
		maxMB = 50
	}
	return &HTTP{
		Client:   client,
		MaxBytes: int64(maxMB) << 20,
	}
}

// limitedReader ограничивает размер читаемого тела ответа.
func limitedReader(r io.Reader, limit int64) io.Reader {
	return io.LimitReader(r, limit)
}

// FetchLines загружает текстовый ресурс и разбивает на строки.
//
// Используется для статических списков (antifilter, static_url).
// Возвращает непустые строки без комментариев.
func (h *HTTP) FetchLines(ctx context.Context, u string) ([]string, error) {
	parsed, err := url.Parse(u)
	if err != nil {
		return nil, fmt.Errorf("parse url: %w", err)
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return nil, fmt.Errorf("unsupported scheme %q", parsed.Scheme)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}

	resp, err := h.Client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetch %s: %w", u, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("fetch %s: %d", u, resp.StatusCode)
	}

	body, err := io.ReadAll(limitedReader(resp.Body, h.MaxBytes))
	if err != nil {
		return nil, fmt.Errorf("read body: %w", err)
	}

	lines := make([]string, 0)
	for _, line := range strings.Split(string(body), "\n") {
		line = strings.TrimSpace(line)
		if line != "" && !strings.HasPrefix(line, "#") {
			lines = append(lines, line)
		}
	}

	return lines, nil
}

// BGPViewASN возвращает префиксы для ASN через BGPView API.
//
// API: https://api.bgpview.io/asn/{asn}/prefixes
// Ответ: {"data":{"ipv4_prefixes":[{"prefix":"1.2.3.0/24"}], ...}}
func BGPViewASN(ctx context.Context, h *HTTP, asn string) ([]string, error) {
	id := strings.TrimPrefix(strings.ToUpper(asn), "AS")
	if id == "" {
		return nil, fmt.Errorf("invalid ASN %q", asn)
	}

	u := "https://api.bgpview.io/asn/" + id + "/prefixes"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, fmt.Errorf("bgpview: create request: %w", err)
	}
	req.Header.Set("Accept", "application/json")

	resp, err := h.Client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("bgpview: request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("bgpview: returned %d", resp.StatusCode)
	}

	var v struct {
		Data struct {
			IPv4Prefixes []struct {
				Prefix string `json:"prefix"`
			} `json:"ipv4_prefixes"`
			IPv6Prefixes []struct {
				Prefix string `json:"prefix"`
			} `json:"ipv6_prefixes"`
		} `json:"data"`
	}

	decoder := json.NewDecoder(limitedReader(resp.Body, h.MaxBytes))
	if err := decoder.Decode(&v); err != nil {
		return nil, fmt.Errorf("bgpview: decode: %w", err)
	}

	out := make([]string, 0, len(v.Data.IPv4Prefixes)+len(v.Data.IPv6Prefixes))
	for _, p := range v.Data.IPv4Prefixes {
		out = append(out, p.Prefix)
	}
	for _, p := range v.Data.IPv6Prefixes {
		out = append(out, p.Prefix)
	}

	if len(out) == 0 {
		return nil, fmt.Errorf("bgpview: no prefixes for %s", asn)
	}

	return out, nil
}
