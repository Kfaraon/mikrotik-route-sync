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
	"time"

	"github.com/hashicorp/go-retryablehttp"

	"github.com/Kfaraon/mikrotik-route-sync/internal/config"
)

type Client struct {
	cfg    config.MikroTikConfig
	http   *retryablehttp.Client
	base   string
}

type Route struct {
	ID           string `json:".id,omitempty"`
	DstAddress   string `json:"dst-address,omitempty"`
	Gateway      string `json:"gateway,omitempty"`
	Distance     string `json:"distance,omitempty"`
	Comment      string `json:"comment,omitempty"`
	RoutingTable string `json:"routing-table,omitempty"`
}

func New(cfg config.MikroTikConfig) *Client {
	scheme := "http"
	if cfg.UseSSL {
		scheme = "https"
	}

	tr := &http.Transport{
		MaxIdleConns:        10,
		IdleConnTimeout:     60 * time.Second,
		TLSHandshakeTimeout: 10 * time.Second,
	}
	if cfg.UseSSL {
		tr.TLSClientConfig = &tls.Config{InsecureSkipVerify: !cfg.VerifySSL}
	}

	retryClient := retryablehttp.NewClient()
	retryClient.HTTPClient = &http.Client{
		Transport: tr,
		Timeout:   30 * time.Second,
	}
	retryClient.RetryMax = 3
	retryClient.RetryWaitMin = 500 * time.Millisecond
	retryClient.RetryWaitMax = 8 * time.Second
	
	// Кастомная логика ретраев: 4xx НЕ ретраим, 5xx и сетевые ошибки ретраим
	retryClient.CheckRetry = func(ctx context.Context, resp *http.Response, err error) (bool, error) {
		if err != nil {
			return true, nil // Сетевая ошибка - ретраим
		}
		// 4xx Client Errors - не ретраим
		if resp.StatusCode >= 400 && resp.StatusCode < 500 {
			return false, nil
		}
		// 5xx Server Errors - ретраим
		return resp.StatusCode >= 500, nil
	}

	return &Client{
		cfg:  cfg,
		http: retryClient,
		base: fmt.Sprintf("%s://%s/rest", scheme, cfg.Host),
	}
}

func (c *Client) do(ctx context.Context, method, path string, body any, out any) error {
	var bodyReader io.Reader
	if body != nil {
		raw, _ := json.Marshal(body)
		bodyReader = bytes.NewReader(raw)
	}

	req, err := retryablehttp.NewRequestWithContext(ctx, method, c.base+path, bodyReader)
	if err != nil {
		return fmt.Errorf("create request: %w", err)
	}
	req.SetBasicAuth(c.cfg.Username, c.cfg.Password)
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("http do: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		raw, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("mikrotik api error %d: %s", resp.StatusCode, string(raw))
	}
	if out != nil {
		return json.NewDecoder(resp.Body).Decode(out)
	}
	return nil
}

// ListRoutes получает маршруты. Использует клиентскую фильтрацию для гарантии comment-exact,
// так как некоторые версии RouterOS v7 REST API могут некорректно обрабатывать параметр exact.
func (c *Client) ListRoutes(ctx context.Context, comment string) ([]Route, error) {
	var allRoutes []Route
	// В RouterOS v7 фильтрация по точному комментарию делается через ?comment=VALUE
	path := fmt.Sprintf("/ip/route?comment=%s", url.QueryEscape(comment))
	err := c.do(ctx, http.MethodGet, path, nil, &allRoutes)
	if err != nil {
		return nil, err
	}

	// Клиентская фильтрация для гарантии точного совпадения
	filtered := make([]Route, 0, len(allRoutes))
	for _, r := range allRoutes {
		if r.Comment == comment {
			filtered = append(filtered, r)
		}
	}
	return filtered, nil
}

func (c *Client) AddRoute(ctx context.Context, r Route) error {
	return c.do(ctx, http.MethodPost, "/ip/route/add", r, nil)
}

func (c *Client) RemoveRoute(ctx context.Context, id string) error {
	return c.do(ctx, http.MethodPost, "/ip/route/remove", map[string]string{"id": id}, nil)
}

func (c *Client) Ping(ctx context.Context) error {
	var out []map[string]any
	return c.do(ctx, http.MethodGet, "/system/identity", nil, &out)
}

func (c *Client) BackupRoutes(ctx context.Context, comment string) ([]Route, error) {
	return c.ListRoutes(ctx, comment)
}
