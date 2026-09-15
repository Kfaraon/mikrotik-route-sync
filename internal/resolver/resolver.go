package resolver

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/netip"
	"strings"
)

type Resolver struct {
	Client   *http.Client
	MaxBytes int64
}

func (r Resolver) ASNForIP(ctx context.Context, ip netip.Addr) (string, error) {
	u := "https://api.bgpview.io/ip/" + ip.String()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	resp, e := r.Client.Do(req)
	if e != nil {
		return "", e
	}
	defer resp.Body.Close()
	var v struct {
		Data struct {
			Prefixes []struct {
				ASN struct {
					ASN int `json:"asn"`
				} `json:"asn"`
			} `json:"prefixes"`
		} `json:"data"`
	}
	if e = json.NewDecoder(io.LimitReader(resp.Body, r.MaxBytes)).Decode(&v); e != nil {
		return "", e
	}
	if len(v.Data.Prefixes) == 0 {
		return "", fmt.Errorf("no ASN for %s", ip)
	}
	return fmt.Sprintf("AS%d", v.Data.Prefixes[0].ASN.ASN), nil
}
func ParseASN(s string) (string, bool) {
	u := strings.ToUpper(strings.TrimSpace(s))
	if !strings.HasPrefix(u, "AS") || len(u) < 3 {
		return "", false
	}
	for _, r := range u[2:] {
		if r < '0' || r > '9' {
			return "", false
		}
	}
	return u, true
}
