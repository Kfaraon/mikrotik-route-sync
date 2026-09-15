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
	"strings"
	"time"

	"github.com/Kfaraon/mikrotik-route-sync/internal/config"
)

type Route struct {
	ID           string `json:".id,omitempty"`
	DstAddress   string `json:"dst-address"`
	Gateway      string `json:"gateway"`
	RoutingTable string `json:"routing-table"`
	Distance     string `json:"distance"`
	Comment      string `json:"comment"`
	Disabled     string `json:"disabled,omitempty"`
}
type Client struct {
	base, user, pass, prefix string
	http                     *http.Client
}

func New(c config.MikroTik) *Client {
	scheme := "http"
	if c.UseSSL {
		scheme = "https"
	}
	tr := &http.Transport{TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12, InsecureSkipVerify: !c.VerifySSL}}
	to := c.Timeout
	if to == 0 {
		to = 30 * time.Second
	}
	return &Client{base: fmt.Sprintf("%s://%s:%d/rest", scheme, c.Host, c.Port), user: c.Username, pass: c.Password, prefix: c.CommentPrefix, http: &http.Client{Transport: tr, Timeout: to}}
}
func (c *Client) do(ctx context.Context, method, path string, body any, out any) error {
	var r io.Reader
	if body != nil {
		b, e := json.Marshal(body)
		if e != nil {
			return e
		}
		r = bytes.NewReader(b)
	}
	req, e := http.NewRequestWithContext(ctx, method, c.base+path, r)
	if e != nil {
		return e
	}
	req.SetBasicAuth(c.user, c.pass)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, e := c.http.Do(req)
	if e != nil {
		return e
	}
	defer resp.Body.Close()
	b, e := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if e != nil {
		return e
	}
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("routeros %s %s: status=%d body=%s", method, path, resp.StatusCode, redact(string(b)))
	}
	if out != nil && len(b) > 0 {
		if e = json.Unmarshal(b, out); e != nil {
			return e
		}
	}
	return nil
}
func redact(s string) string {
	if len(s) > 1024 {
		s = s[:1024]
	}
	return strings.ReplaceAll(s, "password", "[redacted]")
}
func (c *Client) ListServiceRoutes(ctx context.Context, service string) ([]Route, error) {
	comment := c.prefix + ":" + service
	var routes []Route
	path := "/ip/route?comment=" + url.QueryEscape(comment)
	if e := c.do(ctx, http.MethodGet, path, nil, &routes); e != nil {
		return nil, e
	}
	for _, r := range routes {
		if r.Comment != comment {
			return nil, fmt.Errorf("isolation violation: unexpected comment %q", r.Comment)
		}
	}
	return routes, nil
}
func (c *Client) AddRoute(ctx context.Context, r Route) (Route, error) {
	var out Route
	e := c.do(ctx, http.MethodPut, "/ip/route", r, &out)
	return out, e
}
func (c *Client) DeleteRoute(ctx context.Context, id string) error {
	if id == "" || strings.ContainsAny(id, "/?#") {
		return fmt.Errorf("invalid route id")
	}
	return c.do(ctx, http.MethodDelete, "/ip/route/"+url.PathEscape(id), nil, nil)
}
func (c *Client) Ping(ctx context.Context) error {
	var out []Route
	return c.do(ctx, http.MethodGet, "/ip/route?.proplist=.id&.limit=1", nil, &out)
}
