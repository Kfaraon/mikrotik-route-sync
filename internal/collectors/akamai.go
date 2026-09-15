package collectors

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/netip"
	"strings"
	"time"
)

const (
	AkamaiASN = 20940
)

type AkamaiCollector struct {
	httpClient *http.Client
}

func NewAkamaiCollector() Collector {
	return &AkamaiCollector{
		httpClient: &http.Client{Timeout: 30 * time.Second},
	}
}

func (c *AkamaiCollector) Name() string { return "cdn" }

func (c *AkamaiCollector) Collect(ctx context.Context, service string, opts Options) (*Result, error) {
	// Пытаемся получить из публичного списка GitHub
	prefixes, err := c.fetchFromGitHub(ctx)
	if err == nil && len(prefixes) > 0 {
		return &Result{
			Prefixes: prefixes,
			Source:   "github-akamai",
			Method:   "cdn",
		}, nil
	}

	// Fallback: BGPView API для AS20940
	prefixes, err = c.fetchFromBGPView(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to fetch Akamai prefixes: %w", err)
	}

	return &Result{
		Prefixes: prefixes,
		Source:   "bgpview",
		Method:   "cdn",
	}, nil
}

// fetchFromGitHub получает список IP из публичного репозитория Akamai
func (c *AkamaiCollector) fetchFromGitHub(ctx context.Context) ([]netip.Prefix, error) {
	url := "https://raw.githubusercontent.com/Akamai-Open/Akamai-IP-Lists/master/akamai-ip-list.txt"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}

	return c.parseTextList(resp.Body)
}

// fetchFromBGPView получает префиксы через BGPView API
func (c *AkamaiCollector) fetchFromBGPView(ctx context.Context) ([]netip.Prefix, error) {
	url := fmt.Sprintf("https://api.bgpview.io/asn/%d/prefixes", AkamaiASN)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}

	var result struct {
		Data struct {
			IPv4Prefixes []struct {
				Prefix string `json:"prefix"`
			} `json:"ipv4_prefixes"`
			IPv6Prefixes []struct {
				Prefix string `json:"prefix"`
			} `json:"ipv6_prefixes"`
		} `json:"data"`
	}

	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, err
	}

	var prefixes []netip.Prefix
	
	for _, p := range result.Data.IPv4Prefixes {
		if prefix, err := netip.ParsePrefix(p.Prefix); err == nil {
			prefixes = append(prefixes, prefix.Masked())
		}
	}
	
	for _, p := range result.Data.IPv6Prefixes {
		if prefix, err := netip.ParsePrefix(p.Prefix); err == nil {
			prefixes = append(prefixes, prefix.Masked())
		}
	}

	return prefixes, nil
}

// parseTextList парсит текстовый формат (один CIDR на строку)
func (c *AkamaiCollector) parseTextList(r io.Reader) ([]netip.Prefix, error) {
	var prefixes []netip.Prefix
	scanner := bufio.NewScanner(r)
	
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		
		// Удаляем комментарии в конце строки
		if idx := strings.Index(line, "#"); idx >= 0 {
			line = strings.TrimSpace(line[:idx])
		}
		
		if prefix, err := netip.ParsePrefix(line); err == nil {
			prefixes = append(prefixes, prefix.Masked())
		}
	}

	if err := scanner.Err(); err != nil {
		return nil, err
	}

	return prefixes, nil
}
