package resolver

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"time"

	"github.com/Kfaraon/mikrotik-route-sync/internal/storage"
)

type Resolver struct {
	cache  *storage.Cache
	ttl    time.Duration
	client *http.Client
}

func New(cache *storage.Cache, ttl time.Duration) *Resolver {
	return &Resolver{
		cache:  cache,
		ttl:    ttl,
		client: &http.Client{Timeout: 15 * time.Second},
	}
}

func (r *Resolver) ResolveDomain(ctx context.Context, domain string) ([]string, error) {
	addrs, err := net.DefaultResolver.LookupHost(ctx, domain)
	if err != nil {
		return nil, fmt.Errorf("resolve %s: %w", domain, err)
	}
	return addrs, nil
}

func (r *Resolver) GetASN(ctx context.Context, ip string) (int, error) {
	cacheKey := fmt.Sprintf("asn:%s", ip)

	if cached, ok := r.cache.Get(cacheKey); ok {
		var asn int
		if err := json.Unmarshal(cached, &asn); err == nil {
			return asn, nil
		}
	}

	// Используем BGPView API
	url := fmt.Sprintf("https://api.bgpview.io/ip/%s", ip)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return 0, err
	}

	resp, err := r.client.Do(req)
	if err != nil {
		return 0, fmt.Errorf("bgpview request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("bgpview returned %d", resp.StatusCode)
	}

	var result struct {
		Data struct {
			RIRPrefixes []struct {
				ASN struct {
					ASN int `json:"asn"`
				} `json:"rir_allocation"`
			} `json:"rir_prefixes"`
			Prefixes []struct {
				ASN int `json:"asn"`
			} `json:"prefixes"`
		} `json:"data"`
	}

	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return 0, fmt.Errorf("decode bgpview: %w", err)
	}

	asn := 0
	if len(result.Data.Prefixes) > 0 {
		asn = result.Data.Prefixes[0].ASN
	}

	if asn > 0 {
		data, _ := json.Marshal(asn)
		r.cache.Set(cacheKey, data, r.ttl)
	}

	return asn, nil
}

func (r *Resolver) GetASPrefixes(ctx context.Context, asn int) ([]string, error) {
	cacheKey := fmt.Sprintf("prefixes:%d", asn)

	if cached, ok := r.cache.Get(cacheKey); ok {
		var prefixes []string
		if err := json.Unmarshal(cached, &prefixes); err == nil {
			return prefixes, nil
		}
	}

	url := fmt.Sprintf("https://api.bgpview.io/asn/%d/prefixes", asn)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}

	resp, err := r.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("bgpview request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("bgpview returned %d", resp.StatusCode)
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
		return nil, fmt.Errorf("decode prefixes: %w", err)
	}

	var prefixes []string
	for _, p := range result.Data.IPv4Prefixes {
		prefixes = append(prefixes, p.Prefix)
	}
	for _, p := range result.Data.IPv6Prefixes {
		prefixes = append(prefixes, p.Prefix)
	}

	if len(prefixes) > 0 {
		data, _ := json.Marshal(prefixes)
		r.cache.Set(cacheKey, data, r.ttl)
	}

	return prefixes, nil
}
