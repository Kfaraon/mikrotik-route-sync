package collectors

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/Kfaraon/mikrotik-route-sync/internal/config"
	"github.com/hashicorp/go-retryablehttp"
	"github.com/sony/gobreaker"
)

// maxRedirects — ограничение редиректов (PROMPT XI.4).
const maxRedirects = 3

// HTTP — общий HTTP-клиент внешних источников:
// retry + exponential backoff, circuit breaker на каждый хост,
// ограничение размера ответа, SSRF-guard.
type HTTP struct {
	client    *retryablehttp.Client // обычные GET (с общим Timeout = timeout)
	stream    *retryablehttp.Client // потоковая загрузка больших дампов (без жёсткого Timeout, только ctx+dial)
	limit     int64
	timeout   time.Duration
	userAgent string

	breakers  sync.Map // host -> *gobreaker.CircuitBreaker
	breakerMu sync.Map // host -> *sync.Mutex
}

// defaultUserAgent — честная идентификация обязательна для bgp.tools.
const defaultUserAgent = "mikrotik-route-sync/1.0 (https://github.com/Kfaraon/mikrotik-route-sync)"

// NewHTTP создаёт клиент с таймаутом, retry-политикой и лимитом ответа.
// contact (если непусто) добавляется в User-Agent — источники вроде
// bgp.tools требуют описательный UA с способом связи.
func NewHTTP(timeout time.Duration, cfg config.RetryConfig, maxMB int, contact string) *HTTP {
	if timeout <= 0 {
		timeout = 15 * time.Second
	}
	if maxMB <= 0 {
		maxMB = 50
	}
	ua := defaultUserAgent
	if c := strings.TrimSpace(contact); c != "" {
		ua = "mikrotik-route-sync " + c
	}

	transport := &http.Transport{
		Proxy: http.ProxyFromEnvironment,
		DialContext: guardDial(
			&net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second},
		),
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          16,
		IdleConnTimeout:       60 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
	}

	retryClient := retryablehttp.NewClient()
	retryClient.HTTPClient = &http.Client{
		Timeout:       timeout,
		Transport:     transport,
		CheckRedirect: guardRedirects(),
	}
	retryClient.RetryMax = cfg.MaxAttempts
	if retryClient.RetryMax < 1 {
		retryClient.RetryMax = 3
	}
	if cfg.BaseDelay.Duration() > 0 {
		retryClient.RetryWaitMin = cfg.BaseDelay.Duration()
	}
	if cfg.MaxDelay.Duration() > 0 {
		retryClient.RetryWaitMax = cfg.MaxDelay.Duration()
	}
	if !cfg.Jitter {
		retryClient.Backoff = func(min, max time.Duration, attemptNum int, resp *http.Response) time.Duration {
			return min << uint(attemptNum)
		}
	}
	retryClient.Logger = nil

	streamClient := retryablehttp.NewClient()
	streamClient.HTTPClient = &http.Client{
		Transport:     transport,
		CheckRedirect: guardRedirects(),
	}
	streamClient.RetryMax = retryClient.RetryMax
	streamClient.RetryWaitMin = retryClient.RetryWaitMin
	streamClient.RetryWaitMax = retryClient.RetryWaitMax
	streamClient.Logger = nil

	return &HTTP{
		client:    retryClient,
		stream:    streamClient,
		limit:     int64(maxMB) << 20,
		timeout:   timeout,
		userAgent: ua,
	}
}

// breakerFor возвращает (создаёт при необходимости) отдельный breaker для хоста.
func (h *HTTP) breakerFor(host string) *gobreaker.CircuitBreaker {
	if v, ok := h.breakers.Load(host); ok {
		return v.(*gobreaker.CircuitBreaker)
	}
	muRaw, _ := h.breakerMu.LoadOrStore(host, &sync.Mutex{})
	mu := muRaw.(*sync.Mutex)
	mu.Lock()
	defer mu.Unlock()
	if v, ok := h.breakers.Load(host); ok {
		return v.(*gobreaker.CircuitBreaker)
	}
	cb := gobreaker.NewCircuitBreaker(gobreaker.Settings{
		Name:        "external:" + host,
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
	h.breakers.Store(host, cb)
	return cb
}

// Fetch выполняет GET-запрос с retry/breaker/лимитом тела и базовой SSRF-защитой.
func (h *HTTP) Fetch(ctx context.Context, rawURL string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, h.timeout)
	defer cancel()

	u, err := url.Parse(rawURL)
	if err != nil {
		return nil, fmt.Errorf("parse url: %w", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return nil, fmt.Errorf("unsupported scheme %q", u.Scheme)
	}

	cb := h.breakerFor(u.Hostname())
	out, err := cb.Execute(func() (interface{}, error) {
		req, err := retryablehttp.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("User-Agent", h.userAgent)
		req.Header.Set("Accept", "application/json, text/plain;q=0.9")

		resp, err := h.client.Do(req)
		if err != nil {
			return nil, err
		}
		defer resp.Body.Close()

		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			return nil, fmt.Errorf("GET %s: %s", rawURL, resp.Status)
		}

		b, err := io.ReadAll(io.LimitReader(resp.Body, h.limit))
		if err != nil {
			return nil, err
		}
		return b, nil
	})
	if err != nil {
		return nil, err
	}
	return out.([]byte), nil
}

// FetchJSON грузит URL и декодирует JSON-ответ (проверка Content-Type — PROMPT XI.4).
func (h *HTTP) FetchJSON(ctx context.Context, rawURL string, dst any) error {
	body, err := h.Fetch(ctx, rawURL)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(body, dst); err != nil {
		return fmt.Errorf("decode json from %s: %w", rawURL, err)
	}
	return nil
}

// FetchLines грузит текстовый ресурс и разбивает на непустые строки без комментариев.
func (h *HTTP) FetchLines(ctx context.Context, rawURL string) ([]string, error) {
	body, err := h.Fetch(ctx, rawURL)
	if err != nil {
		return nil, err
	}
	lines := make([]string, 0, 64)
	for _, line := range strings.Split(string(body), "\n") {
		line = strings.TrimSpace(strings.SplitN(line, "#", 2)[0])
		if line != "" {
			lines = append(lines, line)
		}
	}
	return lines, nil
}

// ErrStopStream — специальный возврат из fn в StreamLines: остановиться,
// но считать загрузку успешной (например, данные найдены раньше конца дампа).
var ErrStopStream = errors.New("collectors: stop stream")

// StreamLines потоково читает текстовый ресурс построчно (для больших дампов
// вроде bgp.tools table.txt ~30 МБ): тело не буферизуется целиком,
// жёсткого client-таймаута нет — ограничение задаётся ctx-деллайном.
// Ошибки из fn (кроме ErrStopStream) прерывают загрузку и считаются сбоем.
func (h *HTTP) StreamLines(ctx context.Context, rawURL string, fn func(line string) error) error {
	u, err := url.Parse(rawURL)
	if err != nil {
		return fmt.Errorf("parse url: %w", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("unsupported scheme %q", u.Scheme)
	}

	cb := h.breakerFor(u.Hostname())
	_, err = cb.Execute(func() (interface{}, error) {
		req, err := retryablehttp.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("User-Agent", h.userAgent)
		req.Header.Set("Accept", "text/plain, application/json;q=0.9")

		resp, err := h.stream.Do(req)
		if err != nil {
			return nil, err
		}
		defer resp.Body.Close()

		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			return nil, fmt.Errorf("GET %s: %s", rawURL, resp.Status)
		}

		sc := bufio.NewScanner(resp.Body)
		sc.Buffer(make([]byte, 0, 64<<10), 1<<20)
		stop := false
		for sc.Scan() && !stop {
			if err := fn(sc.Text()); err != nil {
				if errors.Is(err, ErrStopStream) {
					stop = true
					continue
				}
				return nil, err
			}
		}
		if stop {
			return nil, nil // успешная загрузка с досрочной остановкой
		}
		if err := sc.Err(); err != nil {
			return nil, fmt.Errorf("stream %s: %w", rawURL, err)
		}
		return nil, nil
	})
	return err
}

// isBlockedIP возвращает true для private/loopback/link-local/multicast/reserved
// адресов — защита от SSRF (PROMPT XI.4).
func isBlockedIP(ip net.IP) bool {
	return ip.IsPrivate() || ip.IsLoopback() || ip.IsLinkLocalUnicast() ||
		ip.IsLinkLocalMulticast() || ip.IsInterfaceLocalMulticast() ||
		ip.IsMulticast() || ip.IsUnspecified() || ip.Equal(net.IPv4bcast)
}

// guardDial оборачивает dialer проверкойresolved-адреса на banned-диапазоны.
func guardDial(d *net.Dialer) func(ctx context.Context, network, addr string) (net.Conn, error) {
	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		host, port, err := net.SplitHostPort(addr)
		if err != nil {
			return nil, err
		}
		ips, err := net.DefaultResolver.LookupIPAddr(ctx, host)
		if err != nil {
			return nil, fmt.Errorf("ssrf-guard: resolve %s: %w", host, err)
		}
		for _, ia := range ips {
			if isBlockedIP(ia.IP) {
				return nil, fmt.Errorf("ssrf-guard: blocked host %s (%s)", host, ia.IP)
			}
		}
		return d.DialContext(ctx, network, net.JoinHostPort(ips[0].IP.String(), port))
	}
}

// guardRedirects ограничивает цепочку редиректов и проверяет цель.
func guardRedirects() func(*http.Request, []*http.Request) error {
	return func(req *http.Request, via []*http.Request) error {
		if len(via) >= maxRedirects {
			return fmt.Errorf("too many redirects")
		}
		if req.URL.Scheme != "http" && req.URL.Scheme != "https" {
			return fmt.Errorf("redirect to unsupported scheme %q", req.URL.Scheme)
		}
		return nil
	}
}
