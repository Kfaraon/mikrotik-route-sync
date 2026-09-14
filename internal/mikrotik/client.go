package mikrotik

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"
	"github.com/sony/gobreaker"
	"github.com/example/mikrotik-route-sync/internal/config"
)

type Client struct {
	cfg  config.MikroTikConfig
	http *http.Client
	cb   *gobreaker.CircuitBreaker
	base string
}

type Route struct {
	ID, DstAddress, Gateway, Distance, Comment, RoutingTable string `json:".id,omitempty" json:"dst-address,omitempty" json:"gateway,omitempty" json:"distance,omitempty" json:"comment,omitempty" json:"routing-table,omitempty"`
}

func New(cfg config.MikroTikConfig) *Client {
	scheme := "http"; if cfg.UseSSL { scheme = "https" }
	tr := &http.Transport{MaxIdleConns: 10, IdleConnTimeout: 60 * time.Second, TLSHandshakeTimeout: 10 * time.Second}
	if cfg.UseSSL { tr.TLSClientConfig = &tls.Config{InsecureSkipVerify: !cfg.VerifySSL} }
	
	return &Client{
		cfg: cfg, http: &http.Client{Timeout: 15 * time.Second, Transport: tr},
		cb: gobreaker.NewCircuitBreaker(gobreaker.Settings{Name: "mikrotik", MaxRequests: 3, Interval: 30 * time.Second, Timeout: 60 * time.Second, ReadyToTrip: func(c gobreaker.Counts) bool { return c.ConsecutiveFailures >= 5 }}),
		base: fmt.Sprintf("%s://%s/rest", scheme, cfg.Host),
	}
}

func (c *Client) do(ctx context.Context, method, path string, body any, out any, canRetry bool) error {
	const maxAttempts = 4
	attempts := 1; if canRetry { attempts = maxAttempts }
	var lastErr error

	for i := 0; i < attempts; i++ {
		if i > 0 {
			backoff := time.Duration(1<<uint(i-1)) * 500 * time.Millisecond
			if backoff > 8*time.Second { backoff = 8 * time.Second }
			select { case <-time.After(backoff): case <-ctx.Done(): return ctx.Err() }
		}
		_, err := c.cb.Execute(func() (any, error) { return nil, c.doOnce(ctx, method, path, body, out) })
		if err == nil { return nil }
		lastErr = err
		
		var he interface{ Status() int } // Упрощённая проверка
		if errors.As(err, &he) { /* логика пропуска 4xx */ }
		if !canRetry || errors.Is(err, gobreaker.ErrOpenState) { return err }
	}
	return lastErr
}

func (c *Client) doOnce(ctx context.Context, method, path string, body any, out any) error {
	var buf io.Reader
	if body != nil { raw, _ := json.Marshal(body); buf = bytes.NewReader(raw) }
	req, _ := http.NewRequestWithContext(ctx, method, c.base+path, buf)
	req.SetBasicAuth(c.cfg.Username, c.cfg.Password)
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.http.Do(req)
	if err != nil { return err }
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		raw, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("mikrotik: %d %s", resp.StatusCode, string(raw))
	}
	if out != nil { return json.NewDecoder(resp.Body).Decode(out) }
	return nil
}

func (c *Client) ListRoutes(ctx context.Context, comment string) ([]Route, error) {
	var routes []Route
	// comment-exact=yes критически важен для RouterOS v7.15+
	path := fmt.Sprintf("/ip/route?comment=%s&comment-exact=yes", url.QueryEscape(comment))
	err := c.do(ctx, http.MethodGet, path, nil, &routes, true)
	return routes, err
}

func (c *Client) AddRoute(ctx context.Context, r Route) error {
	return c.do(ctx, http.MethodPost, "/ip/route/add", r, nil, false)
}

func (c *Client) RemoveRoute(ctx context.Context, id string) error {
	return c.do(ctx, http.MethodPost, "/ip/route/remove", map[string]string{"id": id}, nil, false)
}

func (c *Client) Ping(ctx context.Context) error {
	var out []map[string]any
	return c.do(ctx, http.MethodGet, "/system/identity", nil, &out, true)
}