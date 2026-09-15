package collectors

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
)

// RIPEstatASN получает анонсированные префиксы для ASN через RIPEstat API.
//
// API: https://stat.ripe.net/data/announced-prefixes/data.json?resource=AS{n}
// Ответ: {"data":{"prefixes":[{"prefix":"1.2.3.0/24"}, ...]}}
//
// Используется как fallback при недоступности BGPView.
func RIPEstatASN(ctx context.Context, h *HTTP, asn int) ([]string, error) {
	url := fmt.Sprintf("https://stat.ripe.net/data/announced-prefixes/data.json?resource=AS%d", asn)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("ripestat: create request: %w", err)
	}
	req.Header.Set("User-Agent", "mikrotik-route-sync/1.0")
	req.Header.Set("Accept", "application/json")

	resp, err := h.Client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("ripestat: request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("ripestat: returned %d", resp.StatusCode)
	}

	// Структура ответа RIPEstat API
	var result struct {
		Data struct {
			Prefixes []struct {
				Prefix string `json:"prefix"`
			} `json:"prefixes"`
		} `json:"data"`
	}

	decoder := json.NewDecoder(limitedReader(resp.Body, h.MaxBytes))
	if err := decoder.Decode(&result); err != nil {
		return nil, fmt.Errorf("ripestat: decode: %w", err)
	}

	out := make([]string, 0, len(result.Data.Prefixes))
	for _, p := range result.Data.Prefixes {
		if p.Prefix != "" {
			out = append(out, p.Prefix)
		}
	}

	if len(out) == 0 {
		return nil, fmt.Errorf("ripestat: no prefixes for AS%d", asn)
	}

	return out, nil
}
