package resolver

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"strconv"
	"strings"
	"time"

	"github.com/Kfaraon/mikrotik-route-sync/internal/config"
	"github.com/Kfaraon/mikrotik-route-sync/internal/storage"
)

type Resolver struct {
	cache  *storage.Cache
	ttl    time.Duration
	client *http.Client
	cfg    config.ExternalConfig
}

func New(cache *storage.Cache, ttl time.Duration, cfg config.ExternalConfig) *Resolver {
	return &Resolver{cache: cache, ttl: ttl, client: &http.Client{Timeout: 20 * time.Second}, cfg: cfg}
}

func (r *Resolver) ResolveDomain(ctx context.Context, domain string) ([]netip.Addr, error) {
	domain = strings.TrimSpace(domain)
	if ip, err := netip.ParseAddr(domain); err == nil {
		return []netip.Addr{ip}, nil
	}
	addrs, err := net.DefaultResolver.LookupNetIP(ctx, "ip", domain)
	if err != nil {
		return nil, fmt.Errorf("resolve %s: %w", domain, err)
	}
	out := make([]netip.Addr, 0, len(addrs))
	for _, a := range addrs {
		if a.IsValid() {
			out = append(out, a.Unmap())
		}
	}
	return out, nil
}

func (r *Resolver) ASNByIP(ctx context.Context, ip netip.Addr) (int, error) {
	key := "asn:" + ip.String()
	var cached int
	if r.cache.Get(key, &cached) && cached > 0 {
		return cached, nil
	}
	asn, err := r.asnBGPView(ctx, ip)
	if err != nil {
		asn, err = r.asnRIPE(ctx, ip)
	}
	if err != nil {
		return 0, err
	}
	_ = r.cache.Set(key, asn, r.ttl)
	return asn, nil
}
func (r *Resolver) asnBGPView(ctx context.Context, ip netip.Addr) (int, error) {
	u := strings.TrimRight(r.cfg.BGPViewBase, "/") + "/ip/" + ip.String()
	var payload struct {
		Status string `json:"status"`
		Data   struct {
			Prefixes []struct {
				ASN struct {
					ASN int `json:"asn"`
				} `json:"asn"`
			} `json:"prefixes"`
		} `json:"data"`
	}
	if err := r.getJSON(ctx, u, &payload); err != nil {
		return 0, err
	}
	for _, p := range payload.Data.Prefixes {
		if p.ASN.ASN > 0 {
			return p.ASN.ASN, nil
		}
	}
	return 0, fmt.Errorf("no ASN for %s", ip)
}
func (r *Resolver) asnRIPE(ctx context.Context, ip netip.Addr) (int, error) {
	u := strings.TrimRight(r.cfg.RIPEStatBase, "/") + "/network-info/data.json?resource=" + ip.String()
	var payload struct {
		Data struct {
			ASNs []string `json:"asns"`
		} `json:"data"`
	}
	if err := r.getJSON(ctx, u, &payload); err != nil {
		return 0, err
	}
	for _, s := range payload.Data.ASNs {
		s = strings.TrimPrefix(strings.ToUpper(s), "AS")
		n, _ := strconv.Atoi(s)
		if n > 0 {
			return n, nil
		}
	}
	return 0, fmt.Errorf("no ASN for %s", ip)
}

func (r *Resolver) PrefixesByASN(ctx context.Context, asn int) ([]string, error) {
	key := fmt.Sprintf("prefixes:%d", asn)
	var cached []string
	if r.cache.Get(key, &cached) && len(cached) > 0 {
		return cached, nil
	}
	p, err := r.prefixesBGPView(ctx, asn)
	if err != nil || len(p) == 0 {
		p, err = r.prefixesRIPE(ctx, asn)
	}
	if err != nil {
		return nil, err
	}
	_ = r.cache.Set(key, p, r.ttl)
	return p, nil
}
func (r *Resolver) prefixesBGPView(ctx context.Context, asn int) ([]string, error) {
	u := fmt.Sprintf("%s/asn/%d/prefixes", strings.TrimRight(r.cfg.BGPViewBase, "/"), asn)
	var payload struct {
		Data struct {
			IPv4 []struct {
				Prefix string `json:"prefix"`
			} `json:"ipv4_prefixes"`
			IPv6 []struct {
				Prefix string `json:"prefix"`
			} `json:"ipv6_prefixes"`
		} `json:"data"`
	}
	if err := r.getJSON(ctx, u, &payload); err != nil {
		return nil, err
	}
	out := make([]string, 0, len(payload.Data.IPv4)+len(payload.Data.IPv6))
	for _, x := range payload.Data.IPv4 {
		out = append(out, x.Prefix)
	}
	for _, x := range payload.Data.IPv6 {
		out = append(out, x.Prefix)
	}
	return out, nil
}
func (r *Resolver) prefixesRIPE(ctx context.Context, asn int) ([]string, error) {
	u := fmt.Sprintf("%s/announced-prefixes/data.json?resource=AS%d", strings.TrimRight(r.cfg.RIPEStatBase, "/"), asn)
	var payload struct {
		Data struct {
			Prefixes []struct {
				Prefix string `json:"prefix"`
			} `json:"prefixes"`
		} `json:"data"`
	}
	if err := r.getJSON(ctx, u, &payload); err != nil {
		return nil, err
	}
	out := make([]string, 0, len(payload.Data.Prefixes))
	for _, x := range payload.Data.Prefixes {
		out = append(out, x.Prefix)
	}
	return out, nil
}
func (r *Resolver) getJSON(ctx context.Context, u string, dst any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return err
	}
	if r.cfg.UserAgent != "" {
		req.Header.Set("User-Agent", r.cfg.UserAgent)
	}
	resp, err := r.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("GET %s: %s", u, resp.Status)
	}
	if err := json.NewDecoder(resp.Body).Decode(dst); err != nil {
		return err
	}
	return nil
}
