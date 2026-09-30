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
	"regexp"
	"strings"
	"time"

	"github.com/Kfaraon/mikrotik-route-sync/internal/config"
	"github.com/hashicorp/go-retryablehttp"
	"github.com/sony/gobreaker"
	"golang.org/x/time/rate"
)

// Client — REST-клиент RouterOS v7 для /ip/firewall/address-list:
// retry + circuit breaker + rate limit. Бизнес-логики не содержит.
type Client struct {
	base, user, pass string
	client           *retryablehttp.Client
	breaker          *gobreaker.CircuitBreaker
	limiter          *rate.Limiter
	requestTimeout   time.Duration
}

// Route — структура маршрута из MikroTik RouterOS REST API (/rest/ip/route).
// Используется core.ComputeDiff для инкрементальной синхронизации маршрутов.
type Route struct {
	ID           string `json:".id,omitempty"`
	DstAddress   string `json:"dst-address"`
	Gateway      string `json:"gateway,omitempty"`
	RoutingTable string `json:"routing-table,omitempty"`
	Distance     string `json:"distance,omitempty"` // RouterOS возвращает строку
	Active       bool   `json:"active,omitempty"`
	Dynamic      bool   `json:"dynamic,omitempty"`
	Static       bool   `json:"static,omitempty"`
	Disabled     bool   `json:"disabled,omitempty"`
	Blackhole    bool   `json:"blackhole,omitempty"`
	Comment      string `json:"comment,omitempty"`
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
	retryClient.HTTPClient = &http.Client{Transport: tr}
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
		base:           fmt.Sprintf("%s://%s:%d/rest", scheme, c.Host, c.Port),
		user:           c.Username,
		pass:           c.Password,
		client:         retryClient,
		breaker:        cb,
		limiter:        rate.NewLimiter(lim, int(lim)),
		requestTimeout: c.Timeout.Duration(),
	}
}

// NewClient — конструктор из всего конфига.
func NewClient(cfg *config.Config, log *slog.Logger) (*Client, error) {
	if cfg.MikroTik.Host == "" {
		return nil, fmt.Errorf("mikrotik.host is required")
	}
	if cfg.Firewall.AddressList == "" {
		return nil, fmt.Errorf("firewall.address_list is required")
	}
	return New(cfg.MikroTik, cfg.Retry, log), nil
}

// timeoutContext ограничивает запрос таймаутом mikrotik.timeout.
func (c *Client) timeoutContext(ctx context.Context) (context.Context, context.CancelFunc) {
	d := c.requestTimeout
	if d <= 0 {
		d = 30 * time.Second
	}
	return context.WithTimeout(ctx, d)
}

// doRaw выполняет запрос и возвращает сырое тело успешного ответа.
func (c *Client) doRaw(ctx context.Context, method, path string, body any) ([]byte, error) {
	ctx, cancel := c.timeoutContext(ctx)
	defer cancel()

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

// sensitiveKeysRegex маскирует секреты в сообщениях об ошибках.
var sensitiveKeysRegex = regexp.MustCompile(`(?i)(password|token|secret|api[_-]?key|authorization|cookie)(["\s:=]+)(["']?)([^"'\s,}]+)`)

func redact(s string) string {
	if len(s) > 1024 {
		s = s[:1024]
	}
	return sensitiveKeysRegex.ReplaceAllString(s, "${1}${2}${3}[REDACTED]")
}

// Ping проверяет доступность REST API и право чтения address-list
// (PROMPT IX: /readyz читает /ip/firewall/address-list).
// .limit на address-list ломает ответ (проверено на RouterOS v7) — не используется.
func (c *Client) Ping(ctx context.Context) error {
	var out []map[string]any
	return c.do(ctx, http.MethodGet, addressListPath+"?.proplist=.id", nil, &out)
}

// putID отправляет PUT (создание) и терпеливо разбирает ответ RouterOS:
// объект {"id":"*2F"}, {"id":...} в массиве или пустой ответ.
func (c *Client) putID(ctx context.Context, path string, body any) (string, error) {
	b, err := c.doRaw(ctx, http.MethodPut, path, body)
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
			return "", fmt.Errorf("routeros %s: decode array reply: %w", path, err)
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
		return "", fmt.Errorf("routeros %s: decode object reply: %w", path, err)
	}
	return one.pick(), nil
}

// addReply — форма ответа RouterOS на PUT (создание записи).
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

// deleteByID удаляет запись по .id (формат *XX защищён от инъекций).
func (c *Client) deleteByID(ctx context.Context, path, id string) error {
	if id == "" || strings.ContainsAny(id, "/?#") {
		return fmt.Errorf("invalid entry id %q", id)
	}
	p := fmt.Sprintf("%s?.id=%s", path, id)
	return c.do(ctx, http.MethodDelete, p, nil, nil)
}
