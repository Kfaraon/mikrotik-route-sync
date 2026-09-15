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

// AkamaiCollector собирает IP-диапазоны Akamai CDN (AS20940)
type AkamaiCollector struct {
	apiKey string // опционально, для официального API
}

func NewAkamaiCollector(apiKey string) Collector {
	return &AkamaiCollector{apiKey: apiKey}
}

func (c *AkamaiCollector) Name() string { return "akamai" }

func (c *AkamaiCollector) Collect(ctx context.Context, service string, opts Options) (*Result, error) {
	var prefixes []netip.Prefix
	var source string
	var err error

	// Попытка 1: Публичный список из GitHub (основной источник)
	prefixes, err = c.fetchFromGitHub(ctx)
	if err == nil && len(prefixes) > 0 {
		source = "akamai-github"
		return &Result{
			Prefixes: prefixes,
			Source:   source,
			Method:   "akamai",
		}, nil
	}

	// Попытка 2: Официальный API (если есть ключ)
	if c.apiKey != "" {
		prefixes, err = c.fetchFromOfficialAPI(ctx)
		if err == nil && len(prefixes) > 0 {
			source = "akamai-official-api"
			return &Result{
				Prefixes: prefixes,
				Source:   source,
				Method:   "akamai",
			}, nil
		}
	}

	return nil, fmt.Errorf("failed to fetch Akamai prefixes from all sources: %w", err)
}

// fetchFromGitHub использует публичный список из GitHub
func (c *AkamaiCollector) fetchFromGitHub(ctx context.Context) ([]netip.Prefix, error) {
	client := &http.Client{Timeout: 30 * time.Second}
	
	// Несколько источников на случай недоступности
	urls := []string{
		"https://raw.githubusercontent.com/Akamai-Open/Akamai-IP-Lists/master/akamai-ip-list.txt",
		"https://raw.githubusercontent.com/Akamai-Open/Akamai-IP-Lists/master/akamai-ip-list-ipv4.txt",
	}
	
	var lastErr error
	for _, url := range urls {
		prefixes, err := c.fetchFromURL(ctx, client, url)
		if err == nil && len(prefixes) > 0 {
			return prefixes, nil
		}
		lastErr = err
	}
	
	return nil, fmt.Errorf("all GitHub sources failed: %w", lastErr)
}

// fetchFromURL загружает список CIDR из URL
func (c *AkamaiCollector) fetchFromURL(ctx context.Context, client *http.Client, url string) ([]netip.Prefix, error) {
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
	
	var prefixes []netip.Prefix
	scanner := bufio.NewScanner(resp.Body)
	
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		
		// Пропускаем пустые строки и комментарии
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		
		// Парсим CIDR
		if prefix, err := netip.ParsePrefix(line); err == nil {
			prefixes = append(prefixes, prefix.Masked())
		}
	}
	
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("scan error: %w", err)
	}
	
	return prefixes, nil
}

// fetchFromOfficialAPI использует официальный Akamai API
// Требует API ключ и авторизацию через EdgeGrid
func (c *AkamaiCollector) fetchFromOfficialAPI(ctx context.Context) ([]netip.Prefix, error) {
	// Упрощённая реализация - для production требуется EdgeGrid авторизация
	// https://techdocs.akamai.com/edge-dns/reference/api-overview
	
	client := &http.Client{Timeout: 30 * time.Second}
	url := "https://api.akamai.com/network-lists/v2/networks?listType=IP"
	
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}
	
	// TODO: Добавить EdgeGrid авторизацию для production
	req.Header.Set("Authorization", fmt.Sprintf("Bearer %s", c.apiKey))
	req.Header.Set("Accept", "application/json")
	
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()
	
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("API returned status %d", resp.StatusCode)
	}
	
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read body: %w", err)
	}
	
	var result struct {
		Networks []struct {
			CIDR []string `json:"cidr"`
		} `json:"networks"`
	}
	
	if err := json.Unmarshal(body, &result); err != nil {
		return nil, fmt.Errorf("parse JSON: %w", err)
	}
	
	var prefixes []netip.Prefix
	for _, network := range result.Networks {
		for _, cidr := range network.CIDR {
			if prefix, err := netip.ParsePrefix(cidr); err == nil {
				prefixes = append(prefixes, prefix.Masked())
			}
		}
	}
	
	return prefixes, nil
}
