package collectors

import (
	"context"
	"fmt"
)

// RIPEstatASN получает анонсированные префиксы для ASN через RIPEstat API
// (fallback при недоступности bgp.tools).
//
// API: https://stat.ripe.net/data/announced-prefixes/data.json?resource=AS{n}
func RIPEstatASN(ctx context.Context, h *HTTP, asn int) ([]string, error) {
	if asn <= 0 {
		return nil, fmt.Errorf("invalid ASN %d", asn)
	}
	u := fmt.Sprintf("https://stat.ripe.net/data/announced-prefixes/data.json?resource=AS%d", asn)

	var result struct {
		Data struct {
			Prefixes []struct {
				Prefix string `json:"prefix"`
			} `json:"prefixes"`
		} `json:"data"`
	}
	if err := h.FetchJSON(ctx, u, &result); err != nil {
		return nil, fmt.Errorf("ripestat: %w", err)
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
