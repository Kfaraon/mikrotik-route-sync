package collectors

import (
	"context"
	"encoding/json"
	"fmt"
	"net/netip"
)

type ASNCollector struct{}

func (a *ASNCollector) Name() string { return "asn" }

func (a *ASNCollector) Collect(ctx context.Context, service string, opts Options) (*Result, error) {
	if opts.ASN == 0 {
		return nil, fmt.Errorf("asn collector requires ASN")
	}
	url := fmt.Sprintf("https://api.bgpview.io/asn/%d/prefixes", opts.ASN)
	data, err := fetch(ctx, url)
	if err != nil {
		return nil, err
	}
	var v struct {
		Data struct {
			IPv4 []struct {
				Prefix string `json:"prefix"`
			} `json:"ipv4_prefixes"`
		} `json:"data"`
	}
	if err := json.Unmarshal(data, &v); err != nil {
		return nil, err
	}
	var out []netip.Prefix
	for _, p := range v.Data.IPv4 {
		if pr, err := netip.ParsePrefix(p.Prefix); err == nil {
			out = append(out, pr.Masked())
		}
	}
	if opts.MaxASNPrefixes > 0 && len(out) > opts.MaxASNPrefixes {
		return nil, fmt.Errorf("asn %d has %d prefixes (max %d)", opts.ASN, len(out), opts.MaxASNPrefixes)
	}
	return &Result{Prefixes: out, Source: url, Method: "asn"}, nil
}
