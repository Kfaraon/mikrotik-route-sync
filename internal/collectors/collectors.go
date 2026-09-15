package collectors

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"time"
)

type HTTP struct {
	Client   *http.Client
	MaxBytes int64
}

func NewHTTP(timeout time.Duration, maxMB int) *HTTP {
	tr := &http.Transport{Proxy: http.ProxyFromEnvironment, DialContext: (&net.Dialer{Timeout: timeout}).DialContext, TLSHandshakeTimeout: timeout}
	c := &http.Client{Timeout: timeout, Transport: tr, CheckRedirect: func(req *http.Request, via []*http.Request) error {
		if len(via) >= 3 {
			return http.ErrUseLastResponse
		}
		return nil
	}}
	return &HTTP{Client: c, MaxBytes: int64(maxMB) << 20}
}
func (h *HTTP) FetchLines(ctx context.Context, rawURL string) ([]string, error) {
	u, e := url.Parse(rawURL)
	if e != nil || !(u.Scheme == "https" || u.Scheme == "http") {
		return nil, fmt.Errorf("unsafe url")
	}
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	resp, e := h.Client.Do(req)
	if e != nil {
		return nil, e
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return nil, fmt.Errorf("source status %s", resp.Status)
	}
	r := io.LimitReader(resp.Body, h.MaxBytes)
	var out []string
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		s := strings.TrimSpace(sc.Text())
		if s == "" || strings.HasPrefix(s, "#") {
			continue
		}
		if _, e := netip.ParsePrefix(s); e == nil {
			out = append(out, s)
		}
	}
	return out, sc.Err()
}
func (h *HTTP) FetchFastly(ctx context.Context) ([]string, error) {
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, "https://api.fastly.com/public-ip-list", nil)
	resp, e := h.Client.Do(req)
	if e != nil {
		return nil, e
	}
	defer resp.Body.Close()
	var v struct {
		Addresses     []string `json:"addresses"`
		IPv6Addresses []string `json:"ipv6_addresses"`
	}
	if e = json.NewDecoder(io.LimitReader(resp.Body, h.MaxBytes)).Decode(&v); e != nil {
		return nil, e
	}
	return append(v.Addresses, v.IPv6Addresses...), nil
}
func DNS(ctx context.Context, domains []string, resolverAddr string) ([]string, error) {
	d := net.Dialer{Timeout: 5 * time.Second}
	r := &net.Resolver{PreferGo: true, Dial: func(ctx context.Context, network, address string) (net.Conn, error) {
		return d.DialContext(ctx, "udp", resolverAddr)
	}}
	seen := map[string]bool{}
	var out []string
	for _, domain := range domains {
		ips, e := r.LookupNetIP(ctx, "ip", domain)
		if e != nil {
			continue
		}
		for _, ip := range ips {
			bits := 32
			if ip.Is6() {
				bits = 128
			}
			p := netip.PrefixFrom(ip, bits).String()
			if !seen[p] {
				seen[p] = true
				out = append(out, p)
			}
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("dns returned no addresses")
	}
	return out, nil
}
func BGPViewASN(ctx context.Context, h *HTTP, asn string) ([]string, error) {
	id := strings.TrimPrefix(strings.ToUpper(asn), "AS")
	u := "https://api.bgpview.io/asn/" + id + "/prefixes"
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	resp, e := h.Client.Do(req)
	if e != nil {
		return nil, e
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return nil, fmt.Errorf("bgpview status %s", resp.Status)
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
	if e = json.NewDecoder(io.LimitReader(resp.Body, h.MaxBytes)).Decode(&v); e != nil {
		return nil, e
	}
	out := make([]string, 0, len(v.Data.IPv4Prefixes)+len(v.Data.IPv6Prefixes))
	for _, p := range v.Data.IPv4Prefixes {
		out = append(out, p.Prefix)
	}
	for _, p := range v.Data.IPv6Prefixes {
		out = append(out, p.Prefix)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("bgpview returned no prefixes")
	}
	return out, nil
}
