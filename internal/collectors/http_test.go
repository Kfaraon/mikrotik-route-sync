package collectors

// Тесты HTTP-клиента внешних источников (http.go) без сети:
// транспорт подменяется стабом, SSRF-guard и dial не задействованы.
// Retry ускорен (1 попытка, миллисекунды), чтобы тесты были мгновенными.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/Kfaraon/mikrotik-route-sync/internal/config"
	"github.com/sony/gobreaker"
)

// stubTransport — canned HTTP-ответы без сети, захватывает запрос
// (проверка User-Agent и прочих заголовков).
type stubTransport struct {
	status int
	body   string
	header http.Header
	err    error
	gotReq *http.Request
}

func (s *stubTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	s.gotReq = req
	if s.err != nil {
		return nil, s.err
	}
	return &http.Response{
		StatusCode: s.status,
		Status:     fmt.Sprintf("%d %s", s.status, http.StatusText(s.status)),
		Body:       io.NopCloser(strings.NewReader(s.body)),
		Header:     s.header,
		Request:    req,
	}, nil
}

// fastHTTP — клиент с подменённым транспортом и быстрым retry.
func fastHTTP(t *testing.T, stub *stubTransport) *HTTP {
	t.Helper()
	h := NewHTTP(time.Second, config.RetryConfig{
		MaxAttempts: 1,
		BaseDelay:   config.Duration(time.Millisecond),
		MaxDelay:    config.Duration(time.Millisecond),
	}, 1, "")
	h.client.HTTPClient.Transport = stub
	h.stream.HTTPClient.Transport = stub
	return h
}

func TestNewHTTPDefaults(t *testing.T) {
	h := NewHTTP(0, config.RetryConfig{}, 0, "")
	if h.timeout != 15*time.Second {
		t.Fatalf("timeout=%v", h.timeout)
	}
	if h.limit != 50<<20 {
		t.Fatalf("limit=%d", h.limit)
	}
	if !strings.HasPrefix(h.userAgent, "mikrotik-route-sync/") {
		t.Fatalf("ua=%q", h.userAgent)
	}
	h2 := NewHTTP(time.Second, config.RetryConfig{}, 1, "ops@example.com")
	if !strings.Contains(h2.userAgent, "ops@example.com") {
		t.Fatalf("contact not in UA: %q", h2.userAgent)
	}
}

func TestFetchHappyPathAndUA(t *testing.T) {
	stub := &stubTransport{status: 200, body: "1.0.0.0/24\n"}
	h := fastHTTP(t, stub)
	body, err := h.Fetch(context.Background(), "https://example.com/list.txt")
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if string(body) != "1.0.0.0/24\n" {
		t.Fatalf("body=%q", body)
	}
	if ua := stub.gotReq.Header.Get("User-Agent"); !strings.HasPrefix(ua, "mikrotik-route-sync") {
		t.Fatalf("ua=%q", ua)
	}
}

func TestFetchNon2xxIsError(t *testing.T) {
	for _, status := range []int{404, 500, 301} {
		stub := &stubTransport{status: status}
		h := fastHTTP(t, stub)
		if _, err := h.Fetch(context.Background(), "https://example.com/x"); err == nil {
			t.Fatalf("status %d: expected error", status)
		}
	}
}

func TestFetchRespectsBodyLimit(t *testing.T) {
	stub := &stubTransport{status: 200, body: strings.Repeat("x", 100)}
	h := fastHTTP(t, stub)
	h.limit = 10
	body, err := h.Fetch(context.Background(), "https://example.com/x")
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if len(body) != 10 {
		t.Fatalf("len=%d, want 10", len(body))
	}
}

func TestFetchTransportErrorTripsBreaker(t *testing.T) {
	stub := &stubTransport{err: errors.New("conn refused")}
	h := fastHTTP(t, stub)
	var opened bool
	for i := 0; i < 8; i++ {
		_, err := h.Fetch(context.Background(), "https://flaky.example/x")
		if errors.Is(err, gobreaker.ErrOpenState) {
			opened = true
			break
		}
	}
	if !opened {
		t.Fatal("breaker must open after repeated transport failures")
	}
	// Один breaker на хост: повторный вызов возвращает тот же объект.
	cached := h.breakerFor("flaky.example")
	if h.breakerFor("flaky.example") != cached {
		t.Fatal("breakerFor must return cached breaker")
	}
	if h.breakerFor("other.example") == cached {
		t.Fatal("different hosts must have different breakers")
	}
}

func TestFetchJSON(t *testing.T) {
	stub := &stubTransport{status: 200, body: `{"a": 1}`}
	h := fastHTTP(t, stub)
	var dst struct {
		A int `json:"a"`
	}
	if err := h.FetchJSON(context.Background(), "https://example.com/j", &dst); err != nil {
		t.Fatalf("fetchjson: %v", err)
	}
	if dst.A != 1 {
		t.Fatalf("dst=%+v", dst)
	}
	stub.body = "not json{"
	if err := h.FetchJSON(context.Background(), "https://example.com/j", &dst); err == nil {
		t.Fatal("expected decode error")
	}
}

func TestFetchLinesStripsComments(t *testing.T) {
	stub := &stubTransport{status: 200, body: "# comment\n\n1.0.0.0/24  # inline\n  2.0.0.0/24\n"}
	h := fastHTTP(t, stub)
	lines, err := h.FetchLines(context.Background(), "https://example.com/l")
	if err != nil {
		t.Fatalf("fetchlines: %v", err)
	}
	if len(lines) != 2 || lines[0] != "1.0.0.0/24" || lines[1] != "2.0.0.0/24" {
		t.Fatalf("lines=%q", lines)
	}
}

func TestFetchBadSchemeOffline(t *testing.T) {
	h := NewHTTP(time.Second, config.RetryConfig{MaxAttempts: 1}, 1, "")
	if _, err := h.Fetch(context.Background(), "ftp://example.com/x"); err == nil {
		t.Fatal("expected unsupported scheme error")
	}
	if err := h.FetchJSON(context.Background(), "ftp://example.com/x", &struct{}{}); err == nil {
		t.Fatal("expected unsupported scheme error")
	}
	if err := h.StreamLines(context.Background(), "gopher://example.com/x", func(string) error { return nil }); err == nil {
		t.Fatal("expected unsupported scheme error")
	}
}

func TestFetchBlockedLoopbackOffline(t *testing.T) {
	// Литеральный IP резолвится без DNS — SSRF-guard срабатывает без сети.
	h := NewHTTP(5*time.Second, config.RetryConfig{MaxAttempts: 1, BaseDelay: config.Duration(time.Millisecond)}, 1, "")
	if _, err := h.Fetch(context.Background(), "http://127.0.0.1:9/list.txt"); err == nil {
		t.Fatal("expected ssrf-guard error")
	} else if !strings.Contains(err.Error(), "ssrf-guard") {
		t.Fatalf("want ssrf-guard error, got %v", err)
	}
}

func TestStreamLines(t *testing.T) {
	stub := &stubTransport{status: 200, body: "a\nb\nc\n"}
	h := fastHTTP(t, stub)
	var got []string
	err := h.StreamLines(context.Background(), "https://example.com/big.txt", func(line string) error {
		got = append(got, line)
		return nil
	})
	if err != nil || len(got) != 3 {
		t.Fatalf("got=%q err=%v", got, err)
	}
	// ErrStopStream — успешная досрочная остановка.
	got = nil
	err = h.StreamLines(context.Background(), "https://example.com/big.txt", func(line string) error {
		got = append(got, line)
		return ErrStopStream
	})
	if err != nil || len(got) != 1 {
		t.Fatalf("got=%q err=%v", got, err)
	}
	// Ошибка из fn прерывает загрузку.
	wantErr := errors.New("boom")
	err = h.StreamLines(context.Background(), "https://example.com/big.txt", func(string) error {
		return wantErr
	})
	if !errors.Is(err, wantErr) {
		t.Fatalf("err=%v", err)
	}
}

func TestIsBlockedIP(t *testing.T) {
	blocked := []string{"127.0.0.1", "10.0.0.1", "172.16.0.1", "192.168.1.1",
		"169.254.1.1", "224.0.0.1", "0.0.0.0", "255.255.255.255", "::1", "fe80::1"}
	for _, s := range blocked {
		if !isBlockedIP(net.ParseIP(s)) {
			t.Errorf("%s must be blocked", s)
		}
	}
	for _, s := range []string{"8.8.8.8", "1.1.1.1", "8.47.69.8"} {
		if isBlockedIP(net.ParseIP(s)) {
			t.Errorf("%s must be allowed", s)
		}
	}
}

func TestGuardDialBlocksLiteral(t *testing.T) {
	dial := guardDial(&net.Dialer{Timeout: time.Second})
	if _, err := dial(context.Background(), "tcp", "127.0.0.1:80"); err == nil {
		t.Fatal("expected blocked host error")
	}
	if _, err := dial(context.Background(), "tcp", "bad-addr"); err == nil {
		t.Fatal("expected split error")
	}
}

func TestGuardRedirects(t *testing.T) {
	g := guardRedirects()
	req, _ := http.NewRequest(http.MethodGet, "http://example.com/", nil)
	if err := g(req, nil); err != nil {
		t.Fatalf("plain redirect must pass: %v", err)
	}
	via := make([]*http.Request, 3)
	if err := g(req, via); err == nil {
		t.Fatal("expected too many redirects")
	}
	bad, _ := http.NewRequest(http.MethodGet, "ftp://example.com/", nil)
	if err := g(bad, nil); err == nil {
		t.Fatal("expected unsupported scheme error")
	}
}
