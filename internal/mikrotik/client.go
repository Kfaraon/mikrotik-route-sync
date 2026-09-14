package mikrotik

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/Kfaraon/mikrotik-route-sync/internal/config"
	"github.com/hashicorp/go-retryablehttp"
	"github.com/sony/gobreaker"
)

type Route struct {
	ID           string `json:".id,omitempty"`
	DstAddress   string `json:"dst-address,omitempty"`
	Gateway      string `json:"gateway,omitempty"`
	Distance     string `json:"distance,omitempty"`
	Comment      string `json:"comment,omitempty"`
	RoutingTable string `json:"routing-table,omitempty"`
}
type Client struct {
	cfg  config.MikroTikConfig
	http *retryablehttp.Client
	base string
	cb   *gobreaker.CircuitBreaker
}

func New(cfg config.MikroTikConfig) *Client {
	scheme := "http"
	port := cfg.Port
	if cfg.UseSSL {
		scheme = "https"
		if port == 0 {
			port = 443
		}
	} else if port == 0 {
		port = 80
	}
	host := cfg.Host
	if !strings.Contains(host, ":") {
		host = host + ":" + strconv.Itoa(port)
	}
	tr := &http.Transport{MaxIdleConns: 10, IdleConnTimeout: 60 * time.Second, TLSHandshakeTimeout: 10 * time.Second, TLSClientConfig: &tls.Config{InsecureSkipVerify: !cfg.VerifySSL}}
	rc := retryablehttp.NewClient()
	rc.Logger = nil
	rc.HTTPClient = &http.Client{Transport: tr, Timeout: parseDuration(cfg.Timeout, 30*time.Second)}
	rc.RetryMax = 3
	rc.RetryWaitMin = 500 * time.Millisecond
	rc.RetryWaitMax = 8 * time.Second
	rc.CheckRetry = func(ctx context.Context, resp *http.Response, err error) (bool, error) {
		if err != nil {
			return true, nil
		}
		if resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500 {
			return true, nil
		}
		return false, nil
	}
	cb := gobreaker.NewCircuitBreaker(gobreaker.Settings{Name: "mikrotik-rest", MaxRequests: 1, Interval: 30 * time.Second, Timeout: 15 * time.Second, ReadyToTrip: func(c gobreaker.Counts) bool { return c.ConsecutiveFailures >= 5 }})
	return &Client{cfg: cfg, http: rc, base: fmt.Sprintf("%s://%s/rest", scheme, host), cb: cb}
}
func parseDuration(s string, d time.Duration) time.Duration {
	v, e := time.ParseDuration(s)
	if e != nil || v <= 0 {
		return d
	}
	return v
}
func (c *Client) do(ctx context.Context, method, path string, body any, out any) error {
	_, err := c.cb.Execute(func() (any, error) {
		var raw []byte
		var e error
		if body != nil {
			raw, e = json.Marshal(body)
			if e != nil {
				return nil, e
			}
		}
		req, e := retryablehttp.NewRequestWithContext(ctx, method, c.base+path, bytes.NewReader(raw))
		if e != nil {
			return nil, e
		}
		req.SetBasicAuth(c.cfg.Username, c.cfg.Password)
		req.Header.Set("Content-Type", "application/json")
		resp, e := c.http.Do(req)
		if e != nil {
			return nil, e
		}
		defer resp.Body.Close()
		if resp.StatusCode/100 != 2 {
			b, _ := io.ReadAll(io.LimitReader(resp.Body, 8192))
			return nil, fmt.Errorf("RouterOS REST %s %s: %s: %s", method, path, resp.Status, string(b))
		}
		if out != nil && resp.StatusCode != http.StatusNoContent {
			if e = json.NewDecoder(resp.Body).Decode(out); e != nil && e != io.EOF {
				return nil, e
			}
		}
		return nil, nil
	})
	return err
}
func (c *Client) Ping(ctx context.Context) error {
	var x []map[string]any
	return c.do(ctx, http.MethodGet, "/system/identity", nil, &x)
}
func (c *Client) ListRoutes(ctx context.Context, comment string) ([]Route, error) {
	var routes []Route
	path := "/ip/route?comment=" + url.QueryEscape(comment)
	if err := c.do(ctx, http.MethodGet, path, nil, &routes); err != nil {
		return nil, err
	}
	out := routes[:0]
	for _, r := range routes {
		if r.Comment == comment {
			out = append(out, r)
		}
	}
	return out, nil
}
func (c *Client) AddRoute(ctx context.Context, r Route) error {
	return c.do(ctx, http.MethodPut, "/ip/route", r, nil)
}
func (c *Client) RemoveRoute(ctx context.Context, id string) error {
	if id == "" {
		return fmt.Errorf("route id is empty")
	}
	return c.do(ctx, http.MethodDelete, "/ip/route/"+url.PathEscape(id), nil, nil)
}
func (c *Client) Apply(ctx context.Context, comment string, desired []Route, dryRun bool) (added, removed int, err error) {
	current, err := c.ListRoutes(ctx, comment)
	if err != nil {
		return 0, 0, err
	}

	want := make(map[string]Route, len(desired))
	for _, r := range desired {
		if r.DstAddress == "" {
			return 0, 0, fmt.Errorf("desired route has empty destination")
		}
		if r.Comment != comment {
			return 0, 0, fmt.Errorf("desired route %s has unexpected comment %q", r.DstAddress, r.Comment)
		}
		want[r.DstAddress] = r
	}

	// RouterOS can contain duplicates created by previous/manual runs. Keep at
	// most one exact match and remove every other route within this service's
	// exact comment scope.
	kept := make(map[string]bool, len(current))
	var toRemove []Route
	for _, r := range current {
		w, ok := want[r.DstAddress]
		matches := ok && w.Gateway == r.Gateway && w.RoutingTable == r.RoutingTable && w.Distance == r.Distance && r.Comment == comment
		if matches && !kept[r.DstAddress] {
			kept[r.DstAddress] = true
			continue
		}
		toRemove = append(toRemove, r)
	}

	var toAdd []Route
	for dst, r := range want {
		if !kept[dst] {
			toAdd = append(toAdd, r)
		}
	}

	// Deterministic operation order makes dry-runs, logs and failures easier to
	// reason about.
	sort.Slice(toRemove, func(i, j int) bool { return toRemove[i].DstAddress < toRemove[j].DstAddress })
	sort.Slice(toAdd, func(i, j int) bool { return toAdd[i].DstAddress < toAdd[j].DstAddress })

	if dryRun {
		return len(toAdd), len(toRemove), nil
	}
	backup := append([]Route(nil), current...)
	mutated := false
	for _, r := range toRemove {
		if err = c.RemoveRoute(ctx, r.ID); err != nil {
			if mutated {
				_ = c.rollback(ctx, comment, backup)
			}
			return added, removed, fmt.Errorf("remove %s failed: %w", r.DstAddress, err)
		}
		mutated = true
		removed++
	}
	for _, r := range toAdd {
		if err = c.AddRoute(ctx, r); err != nil {
			if mutated || added > 0 {
				_ = c.rollback(ctx, comment, backup)
			}
			return added, removed, fmt.Errorf("add %s failed, rollback attempted: %w", r.DstAddress, err)
		}
		mutated = true
		added++
	}
	return added, removed, nil
}

func (c *Client) rollback(ctx context.Context, comment string, backup []Route) error {
	cur, err := c.ListRoutes(ctx, comment)
	if err == nil {
		for _, r := range cur {
			_ = c.RemoveRoute(ctx, r.ID)
		}
	}
	for _, r := range backup {
		r.ID = ""
		if e := c.AddRoute(ctx, r); e != nil {
			err = e
		}
	}
	return err
}
