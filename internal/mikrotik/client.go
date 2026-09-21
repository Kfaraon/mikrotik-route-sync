package mikrotik

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/Kfaraon/mikrotik-route-sync/internal/config"
	"github.com/hashicorp/go-retryablehttp"
	"github.com/sony/gobreaker"
	"golang.org/x/time/rate"
)

// Route — маршрут в RouterOS (формат REST API).
type Route struct {
	ID           string `json:".id,omitempty"`
	DstAddress   string `json:"dst-address"`
	Gateway      string `json:"gateway"`
	RoutingTable string `json:"routing-table,omitempty"`
	Distance     string `json:"distance,omitempty"`
	Comment      string `json:"comment,omitempty"`
	Disabled     string `json:"disabled,omitempty"`
}

// Client — REST-клиент RouterOS v7: retry + circuit breaker + rate limit.
// Бизнес-логики не содержит (PROMPT X.10.1).
type Client struct {
	base, user, pass, prefix string
	client                   *retryablehttp.Client
	breaker                  *gobreaker.CircuitBreaker
	limiter                  *rate.Limiter
}

// New создаёт клиент из конфигурации.
func New(c config.MikroTikConfig, rc config.RetryConfig, log *slog.Logger) *Client {
	scheme := "http"
	if c.UseSSL {
		scheme = "https"
	}

	if !c.VerifySSL && log != nil {
		log.Warn("mikrotik verify_ssl disabled — acceptable only in trusted home networks",
			"host", c.Host)
	}

	tr := &http.Transport{
		Proxy: http.ProxyFromEnvironment,
		TLSClientConfig: &tls.Config{
			MinVersion:         tls.VersionTLS12,
			InsecureSkipVerify: !c.VerifySSL,
		},
	}

	retryClient := retryablehttp.NewClient()
	retryClient.HTTPClient = &http.Client{
		Transport: tr,
		Timeout:   c.Timeout.Duration(),
	}
	retryClient.RetryMax = rc.MaxAttempts
	if retryClient.RetryMax < 1 {
		retryClient.RetryMax = 3
	}
	if rc.BaseDelay.Duration() > 0 {
		retryClient.RetryWaitMin = rc.BaseDelay.Duration()
	}
	if rc.MaxDelay.Duration() > 0 {
		retryClient.RetryWaitMax = rc.MaxDelay.Duration()
	}
	if !rc.Jitter {
		retryClient.Backoff = func(min, max time.Duration, attemptNum int, resp *http.Response) time.Duration {
			return min << uint(attemptNum)
		}
	}
	retryClient.Logger = nil

	cb := gobreaker.NewCircuitBreaker(gobreaker.Settings{
		Name:        "mikrotik",
		MaxRequests: 3,
		Interval:    60 * time.Second,
		Timeout:     60 * time.Second,
		ReadyToTrip: func(counts gobreaker.Counts) bool {
			if counts.Requests < 5 {
				return false
			}
			return float64(counts.TotalFailures)/float64(counts.Requests) >= 0.6
		},
	})

	lim := rate.Limit(c.RateLimit)
	if lim <= 0 {
		lim = 20
	}

	return &Client{
		base:    fmt.Sprintf("%s://%s:%d/rest", scheme, c.Host, c.Port),
		user:    c.Username,
		pass:    c.Password,
		prefix:  c.CommentPrefix,
		client:  retryClient,
		breaker: cb,
		limiter: rate.NewLimiter(lim, int(lim)),
	}
}

// NewClient — конструктор из всего конфига.
func NewClient(cfg *config.Config, log *slog.Logger) (*Client, error) {
	if cfg.MikroTik.Host == "" {
		return nil, fmt.Errorf("mikrotik.host is required")
	}
	return New(cfg.MikroTik, cfg.Retry, log), nil
}

// doRaw выполняет запрос и возвращает сырое тело успешного (2xx, без
// встроенной ошибки API) ответа.
func (c *Client) doRaw(ctx context.Context, method, path string, body any) ([]byte, error) {
	if err := c.limiter.Wait(ctx); err != nil {
		return nil, fmt.Errorf("rate limit wait: %w", err)
	}

	out, err := c.breaker.Execute(func() (interface{}, error) {
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

		// RouterOS REST может вернуть 200 с телом {"detail": "...", "error": ...}
		if len(b) > 0 {
			var probe map[string]any
			if json.Unmarshal(b, &probe) == nil {
				if _, hasErr := probe["error"]; hasErr {
					return nil, fmt.Errorf("routeros %s %s: api error: %s", method, path, redact(string(b)))
				}
			}
		}
		return b, nil
	})
	if err != nil {
		return nil, err
	}
	return out.([]byte), nil
}

// do выполняет запрос и декодирует успешный ответ в out.
func (c *Client) do(ctx context.Context, method, path string, body any, out any) error {
	b, err := c.doRaw(ctx, method, path, body)
	if err != nil {
		return err
	}
	if out != nil && len(b) > 0 && string(b) != "[]" {
		if err := json.Unmarshal(b, out); err != nil {
			return fmt.Errorf("routeros %s %s: decode: %w", method, path, err)
		}
	}
	return nil
}

func (c *Client) doWithPagination(ctx context.Context, path string, limit int) ([]Route, error) {
	var all []Route
	skip := 0
	for {
		paginatedPath := path
		if strings.Contains(path, "?") {
			paginatedPath += fmt.Sprintf("&.skip=%d&.limit=%d", skip, limit)
		} else {
			paginatedPath += fmt.Sprintf("?.skip=%d&.limit=%d", skip, limit)
		}
		var page []Route
		if err := c.do(ctx, http.MethodGet, paginatedPath, nil, &page); err != nil {
			return nil, err
		}
		all = append(all, page...)
		if len(page) < limit {
			break
		}
		skip += limit
		if skip > 100000 {
			break
		}
	}
	return all, nil
}

// sensitiveKeysRegex маскирует секреты в сообщениях об ошибках.
var sensitiveKeysRegex = regexp.MustCompile(`(?i)(password|token|secret|api[_-]?key|authorization|cookie)(["\s:=]+)(["']?)([^"'\s,}]+)`)

func redact(s string) string {
	if len(s) > 1024 {
		s = s[:1024]
	}
	return sensitiveKeysRegex.ReplaceAllString(s, "${1}${2}${3}[REDACTED]")
}

// ListServiceRoutes возвращает ТОЛЬКО маршруты сервиса (comment == AUTO:<service>).
//
// Фильтрация выполняется ИСКЛЮЧИТЕЛЬНО на клиенте: server-side фильтр
// "?comment=..." в RouterOS REST ненадёжен (может молча вернуть пустой список
// при URL-кодированном значении — реальный случай 2026-09-20, из-за чего diff
// не видел существующие маршруты и создавал дубли). Точное сравнение комментария
// — гарантия изоляции сервисов (PROMPT I.1, III.6).
func (c *Client) ListServiceRoutes(ctx context.Context, service string) ([]Route, error) {
	comment := c.prefix + ":" + service

	routes, err := c.doWithPagination(ctx, "/ip/route", 1000)
	if err != nil {
		return nil, err
	}

	out := make([]Route, 0, len(routes))
	for _, r := range routes {
		if r.Comment == comment {
			out = append(out, r)
		}
	}
	return out, nil
}

// addReply — форма ответа RouterOS на PUT (создание): объект {"id":"*2F"}
// (иногда {".id":...} или {"done":...} в разных версиях), исторически
// встречался и массив [{...}].
type addReply struct {
	ID    string `json:"id"`
	DotID string `json:".id"`
	Done  string `json:"done"`
}

func (a addReply) pick() string {
	for _, s := range []string{a.ID, a.DotID, a.Done} {
		if s != "" {
			return s
		}
	}
	return ""
}

// AddRoute создаёт маршрут (PUT /rest/ip/route) и возвращает новый .id
// (может быть пустым, если RouterOS не вернул идентификатор — не ошибка).
func (c *Client) AddRoute(ctx context.Context, r Route) (string, error) {
	b, err := c.doRaw(ctx, http.MethodPut, "/ip/route", r)
	if err != nil {
		return "", err
	}

	b = bytes.TrimSpace(b)
	if len(b) == 0 || string(b) == "[]" {
		return "", nil
	}

	if b[0] == '[' {
		var list []addReply
		if err := json.Unmarshal(b, &list); err != nil {
			return "", fmt.Errorf("routeros add route: decode array reply: %w", err)
		}
		for _, a := range list {
			if id := a.pick(); id != "" {
				return id, nil
			}
		}
		return "", nil
	}

	var one addReply
	if err := json.Unmarshal(b, &one); err != nil {
		return "", fmt.Errorf("routeros add route: decode object reply: %w", err)
	}
	return one.pick(), nil
}

// DeleteRoute удаляет маршрут по .id (например "*1A").
func (c *Client) DeleteRoute(ctx context.Context, id string) error {
	if id == "" || strings.ContainsAny(id, "/?#") {
		return fmt.Errorf("invalid route id %q", id)
	}
	path := fmt.Sprintf("/ip/route?.id=%s", url.QueryEscape(id))
	return c.do(ctx, http.MethodDelete, path, nil, nil)
}

// Ping проверяет доступность REST API.
func (c *Client) Ping(ctx context.Context) error {
	var out []Route
	return c.do(ctx, http.MethodGet, "/ip/route?.proplist=.id&.limit=1", nil, &out)
}
