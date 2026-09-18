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
	"regexp"
	"strings"
	"time"

	"github.com/Kfaraon/mikrotik-route-sync/internal/config"
	"github.com/hashicorp/go-retryablehttp"
	"github.com/sony/gobreaker"
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
	client                   *retryablehttp.Client
	breaker                  *gobreaker.CircuitBreaker
}

func New(c config.MikroTik, cfg config.RetryConfig) *Client {
	scheme := "http"
	if c.UseSSL {
		scheme = "https"
	}
	tr := &http.Transport{TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12, InsecureSkipVerify: !c.VerifySSL}}
	
	retryClient := retryablehttp.NewClient()
	retryClient.HTTPClient.Transport = tr
	retryClient.HTTPClient.Timeout = c.Timeout.Duration()
	retryClient.RetryMax = cfg.MaxAttempts
	if cfg.BaseDelay.Duration() > 0 {
		retryClient.RetryWaitMin = cfg.BaseDelay.Duration()
	}
	if cfg.MaxDelay.Duration() > 0 {
		retryClient.RetryWaitMax = cfg.MaxDelay.Duration()
	}
	if !cfg.Jitter {
		retryClient.Backoff = func(min, max time.Duration, attemptNum int, resp *http.Response) time.Duration {
			mult := 1 << uint(attemptNum)
			return time.Duration(mult) * min
		}
	}
	
	cb := gobreaker.NewCircuitBreaker(gobreaker.Settings{
		Name:        "mikrotik",
		MaxRequests: 3,
		Interval:    60 * time.Second,
		Timeout:     60 * time.Second,
		ReadyToTrip: func(counts gobreaker.Counts) bool {
			failureRatio := float64(counts.TotalFailures) / float64(counts.Requests)
			return counts.Requests >= 5 && failureRatio >= 0.6
		},
	})

	return &Client{
		base:    fmt.Sprintf("%s://%s:%d/rest", scheme, c.Host, c.Port),
		user:    c.Username,
		pass:    c.Password,
		prefix:  c.CommentPrefix,
		client:  retryClient,
		breaker: cb,
	}
}

func (c *Client) do(ctx context.Context, method, path string, body any, out any) error {
	var err error
	
	_, err = c.breaker.Execute(func() (interface{}, error) {
		var r io.Reader
		if body != nil {
			b, e := json.Marshal(body)
			if e != nil {
				return nil, e
			}
			r = bytes.NewReader(b)
		}

		req, e := retryablehttp.NewRequestWithContext(ctx, method, c.base+path, r)
		if e != nil {
			return nil, e
		}
		req.SetBasicAuth(c.user, c.pass)
		req.Header.Set("Accept", "application/json")
		if body != nil {
			req.Header.Set("Content-Type", "application/json")
		}

		resp, e := c.client.Do(req)
		if e != nil {
			return nil, e
		}
		defer resp.Body.Close()
		
		b, e := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
		if e != nil {
			return nil, e
		}
		
		if resp.StatusCode/100 != 2 {
			return nil, fmt.Errorf("routeros %s %s: status=%d body=%s", method, path, resp.StatusCode, redact(string(b)))
		}
		
		if out != nil && len(b) > 0 {
			if e = json.Unmarshal(b, out); e != nil {
				return nil, e
			}
		}
		return nil, nil
	})
	
	return err
}

func (c *Client) doWithPagination(ctx context.Context, method, path string, limit int) ([]json.RawMessage, error) {
	var allResults []json.RawMessage
	skip := 0
	for {
		paginatedPath := path
		if strings.Contains(path, "?") {
			paginatedPath += fmt.Sprintf("&.skip=%d&.limit=%d", skip, limit)
		} else {
			paginatedPath += fmt.Sprintf("?.skip=%d&.limit=%d", skip, limit)
		}
		var page []json.RawMessage
		if err := c.do(ctx, method, paginatedPath, nil, &page); err != nil {
			return nil, err
		}
		allResults = append(allResults, page...)
		if len(page) < limit {
			break
		}
		skip += limit
		if skip > 100000 {
			break
		}
	}
	return allResults, nil
}

// ИСПРАВЛЕНИЕ: redact использует regex для маскировки всех чувствительных ключей
var sensitiveKeysRegex = regexp.MustCompile(`(?i)(password|token|secret|api[_-]?key|authorization|cookie)(["\s:=]+)(["']?)([^"'\s,}]+)(["']?)`)

func redact(s string) string {
	if len(s) > 1024 {
		s = s[:1024]
	}
	return sensitiveKeysRegex.ReplaceAllString(s, "${1}${2}${3}[REDACTED]${5}")
}

func (c *Client) ListServiceRoutes(ctx context.Context, service string) ([]Route, error) {
	comment := c.prefix + ":" + service
	path := "/ip/route?comment=" + url.QueryEscape(comment)
	rawRoutes, err := c.doWithPagination(ctx, http.MethodGet, path, 1000)
	if err != nil {
		return nil, err
	}
	var routes []Route
	for _, raw := range rawRoutes {
		var route Route
		if err := json.Unmarshal(raw, &route); err != nil {
			return nil, fmt.Errorf("failed to unmarshal route: %w", err)
		}
		routes = append(routes, route)
	}
	for _, r := range routes {
		if r.Comment != comment {
			return nil, fmt.Errorf("isolation violation: unexpected comment %q (expected %q)", r.Comment, comment)
		}
	}
	return routes, nil
}

func (c *Client) AddRoute(ctx context.Context, r Route) (Route, error) {
	var out Route
	e := c.do(ctx, http.MethodPut, "/ip/route", r, &out)
	return out, e
}

// ИСПРАВЛЕНИЕ: DeleteRoute корректно работает с ID типа *1A
func (c *Client) DeleteRoute(ctx context.Context, id string) error {
	if id == "" || strings.ContainsAny(id, "/?#") {
		return fmt.Errorf("invalid route id")
	}
	// Используем query параметр вместо path escape
	path := fmt.Sprintf("/ip/route?.id=%s", url.QueryEscape(id))
	return c.do(ctx, http.MethodDelete, path, nil, nil)
}

func (c *Client) Ping(ctx context.Context) error {
	var out []Route
	return c.do(ctx, http.MethodGet, "/ip/route?.proplist=.id&.limit=1", nil, &out)
}

func NewClient(c *config.Config) (*Client, error) {
	return New(c.MikroTik, c.Retry), nil
}
