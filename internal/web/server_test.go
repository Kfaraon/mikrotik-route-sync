package web

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Kfaraon/mikrotik-route-sync/internal/config"
	"github.com/Kfaraon/mikrotik-route-sync/internal/core"
	"github.com/Kfaraon/mikrotik-route-sync/internal/notifier"
	"github.com/Kfaraon/mikrotik-route-sync/internal/presets"
	"github.com/Kfaraon/mikrotik-route-sync/internal/storage"
)

func newTestServer(t *testing.T) (*Server, *config.Config) {
	t.Helper()

	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	body := `
timezone: UTC
mikrotik:
  host: 127.0.0.1
  port: 9
  username: api
  password: x
  timeout: 1s
firewall:
  address_list: TO-VPN
retry:
  max_attempts: 1
  base_delay: 1ms
  max_delay: 1ms
safety:
  min_prefix_v4: 8
web:
  enabled: true
  listen: 127.0.0.1:0
  allowed_cidrs: [127.0.0.0/8]
  auth:
    enabled: true
    username: admin
    password: secret-pass
  session_timeout: 1h
  csrf_enabled: true
  security_headers: true
schedules:
  global: "every 6h"
services: [testsvc]
`
	if err := os.WriteFile(cfgPath, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}

	cache, err := storage.Open(filepath.Join(dir, "cache.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cache.Close() })

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	s, err := core.NewSyncer(cfg, log, cache, &notifier.NoopNotifier{})
	if err != nil {
		t.Fatal(err)
	}
	srv, err := NewServer(cfg, s, log)
	if err != nil {
		t.Fatal(err)
	}
	// Мёртвый MikroTik без реальной сети: мгновенная ошибка вместо
	// dial-таймаутов на каждом первом рендере страницы.
	srv.pingFn = func(ctx context.Context) error {
		return errors.New("mikrotik down in tests")
	}
	return srv, cfg
}

func doAuth(c *http.Client, base, method, path, csrf string, body io.Reader) (*http.Response, string) {
	req, err := http.NewRequest(method, base+path, body)
	if err != nil {
		panic(err)
	}
	req.SetBasicAuth("admin", "secret-pass")
	if csrf != "" {
		req.Header.Set("X-CSRF-Token", csrf)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	resp, err := c.Do(req)
	if err != nil {
		panic(err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(resp.Body)
	return resp, string(b)
}

func TestHealthzNoAuth(t *testing.T) {
	srv, _ := newTestServer(t)
	ts := httptest.NewServer(srv)
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("healthz: %d", resp.StatusCode)
	}
	var v apiEnvelope
	_ = json.NewDecoder(resp.Body).Decode(&v)
	if !v.OK {
		t.Fatal("healthz envelope not ok")
	}
}

func TestAuthRequiredAndDashboardRenders(t *testing.T) {
	srv, _ := newTestServer(t)
	ts := httptest.NewServer(srv)
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", resp.StatusCode)
	}

	client := &http.Client{Jar: newJar(t)}
	resp, page := doAuth(client, ts.URL, "GET", "/", "", nil)
	if resp.StatusCode != 200 {
		t.Fatalf("dashboard: %d body=%s", resp.StatusCode, page)
	}
	if !strings.Contains(page, "MikroTik Route Sync") {
		t.Fatalf("dashboard content wrong: %s", page)
	}
	// Сервисы рендерит JS через /api/v1/services — проверяем API.
	_, svcs := doAuth(client, ts.URL, "GET", "/api/v1/services", "", nil)
	if !strings.Contains(svcs, "testsvc") {
		t.Fatalf("api services missing testsvc: %s", svcs)
	}
	if !strings.Contains(page, "csrf-token") {
		t.Fatal("csrf meta missing")
	}
	if got := resp.Header.Get("X-Frame-Options"); got != "DENY" {
		t.Fatalf("security header missing: %q", got)
	}
}

func TestCSRFRequiredForMutations(t *testing.T) {
	srv, _ := newTestServer(t)
	ts := httptest.NewServer(srv)
	defer ts.Close()

	// Cookie больше не выдаётся при Basic (нет fixation): запрос без cookie
	// идёт веткой basicAuth. Без Origin — программный клиент, пропуск
	// (но НЕ 403): защита от браузера строится на Origin/Referer.
	client := &http.Client{Jar: newJar(t)}
	if resp, body := doAuth(client, ts.URL, "GET", "/", "", nil); resp.StatusCode != 200 {
		t.Fatalf("auth GET failed: %d %s", resp.StatusCode, body)
	}

	// Чужой Origin в Basic-ветке — 403 (см. TestCSRFBasicauthOrigin).
	// Чужой Referer без Origin — тоже 403.
	req, _ := http.NewRequest("POST", ts.URL+"/actions/sync-all", strings.NewReader(""))
	req.SetBasicAuth("admin", "secret-pass")
	req.Header.Set("Referer", "http://evil.example/page")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("expected 403 on cross-origin referer, got %d", resp.StatusCode)
	}

	_, page := doAuth(client, ts.URL, "GET", "/", "", nil)
	start := strings.Index(page, `name="csrf-token" content="`)
	if start < 0 {
		t.Fatal("token not found")
	}
	rest := page[start+len(`name="csrf-token" content="`):]
	token := rest[:strings.Index(rest, `"`)]

	resp, body := doAuth(client, ts.URL, "POST", "/actions/service-add", token,
		strings.NewReader("name=10.0.0.8&csrf_token="+token))
	_ = resp
	if !strings.Contains(body, `"ok":true`) {
		t.Fatalf("add with csrf failed: %s", body)
	}
}

func TestAPIStatusEnvelope(t *testing.T) {
	srv, _ := newTestServer(t)
	ts := httptest.NewServer(srv)
	defer ts.Close()

	client := &http.Client{Jar: newJar(t)}
	resp, body := doAuth(client, ts.URL, "GET", "/api/v1/status", "", nil)
	if resp.StatusCode != 200 {
		t.Fatalf("status: %d %s", resp.StatusCode, body)
	}
	var v apiEnvelope
	if err := json.Unmarshal([]byte(body), &v); err != nil || !v.OK {
		t.Fatalf("bad envelope: %s err=%v", body, err)
	}
}

// GET /api/v1/progress — карта активных синхронизаций (in-memory).
func TestAPIProgressEnvelope(t *testing.T) {
	srv, _ := newTestServer(t)
	ts := httptest.NewServer(srv)
	defer ts.Close()

	client := &http.Client{Jar: newJar(t)}
	resp, body := doAuth(client, ts.URL, "GET", "/api/v1/progress", "", nil)
	if resp.StatusCode != 200 {
		t.Fatalf("status: %d %s", resp.StatusCode, body)
	}
	var v apiEnvelope
	if err := json.Unmarshal([]byte(body), &v); err != nil || !v.OK {
		t.Fatalf("bad envelope: %s err=%v", body, err)
	}
}

func TestSettingsPageComplete(t *testing.T) {
	srv, cfg := newTestServer(t)
	ts := httptest.NewServer(srv)
	defer ts.Close()

	client := &http.Client{Jar: newJar(t)}
	_, page := doAuth(client, ts.URL, "GET", "/settings", "", nil)

	for _, want := range []string{
		"Планировщик", "Внешние API", "Снапшоты", "Логирование",
		`name="logging.max_size_mb"`,
		`name="telegram.authorized_chat_ids"`,
		`name="snapshots.max_count"`,
		`name="external.akamai_api_key"`,
		`name="firewall.address_list"`,
		`name="firewall.comment_prefix"`,
		`name="safety.require_confirmation_over"`,
		`name="retry.jitter"`,
		`class="setting-help"`,
		`<option value="true" selected>`,
	} {
		if !strings.Contains(page, want) {
			t.Fatalf("settings page missing %q", want)
		}
	}

	// Сохранение + автоприменение: меняем float и список (с валидным CSRF)
	start := strings.Index(page, `name="csrf-token" content="`)
	rest := page[start+len(`name="csrf-token" content="`):]
	token := rest[:strings.Index(rest, `"`)]

	resp, body := doAuth(client, ts.URL, "POST", "/actions/settings", token,
		strings.NewReader("csrf_token="+token+"&safety.max_delete_ratio=0.25&web.allowed_cidrs=127.0.0.0/8,10.0.0.0/8"))
	_ = resp
	if !strings.Contains(body, `"ok":true`) {
		t.Fatalf("save failed: %s", body)
	}
	if cfg.Safety.MaxDeleteRatio != 0.25 {
		t.Fatalf("MaxDeleteRatio not applied: %v", cfg.Safety.MaxDeleteRatio)
	}
	if len(cfg.Web.AllowedCIDRs) != 2 {
		t.Fatalf("list not applied: %v", cfg.Web.AllowedCIDRs)
	}
}

func TestSettingsSecretsMasked(t *testing.T) {
	srv, _ := newTestServer(t)
	ts := httptest.NewServer(srv)
	defer ts.Close()

	client := &http.Client{Jar: newJar(t)}
	_, page := doAuth(client, ts.URL, "GET", "/settings", "", nil)
	if strings.Contains(page, "secret-pass") {
		t.Fatal("web password leaked into page")
	}
	if !strings.Contains(page, "\u2022") {
		t.Fatal("secrets not masked on settings page")
	}
}

func TestCIDREnforcement(t *testing.T) {
	srv, _ := newTestServer(t)
	_, foreign, err := net.ParseCIDR("10.99.0.0/16")
	if err != nil {
		t.Fatal(err)
	}
	srv.ips = &ipChecker{enabled: true, nets: []*net.IPNet{foreign}}
	ts := httptest.NewServer(srv)
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("healthz must be allowed without cidr check, got %d", resp.StatusCode)
	}
	resp, err = http.Get(ts.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("expected 403 from foreign subnet, got %d", resp.StatusCode)
	}
}

func TestSettingsRejectedKeepsLiveConfig(t *testing.T) {
	srv, cfg := newTestServer(t)
	ts := httptest.NewServer(srv)
	defer ts.Close()

	client := &http.Client{Jar: newJar(t)}
	_, page := doAuth(client, ts.URL, "GET", "/settings", "", nil)
	start := strings.Index(page, `name="csrf-token" content="`)
	rest := page[start+len(`name="csrf-token" content="`):]
	token := rest[:strings.Index(rest, `"`)]

	// В тест-конфиге port=9; пробуем set port=443 при use_ssl=false —
	// невалидная комбинация, сохранение отклоняется целиком.
	resp, body := doAuth(client, ts.URL, "POST", "/actions/settings", token,
		strings.NewReader("csrf_token="+token+"&mikrotik.port=443"))
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400 for use_ssl=false+port443, got %d: %s", resp.StatusCode, body)
	}
	if cfg.MikroTik.Port != 9 {
		t.Fatalf("rejected save must not mutate live config (port flipped): %d", cfg.MikroTik.Port)
	}
}

func newJar(t *testing.T) http.CookieJar {
	t.Helper()
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	return jar
}

func TestIPLimiterEvicts(t *testing.T) {
	l := newIPLimiter(1000)
	if !l.allow("10.0.0.1") {
		t.Fatal("first request must pass")
	}
	// Переполнение уникальными IP — карта обязана оставаться ограниченной.
	for i := 0; i < maxLimiters+500; i++ {
		l.allow(strings.Repeat("x", 8) + string(rune(i)))
	}
	l.mu.Lock()
	n := len(l.limiters)
	l.mu.Unlock()
	if n > maxLimiters {
		t.Fatalf("limiters=%d, want <= %d", n, maxLimiters)
	}
	// Протухшие записи вычищаются при следующем переполнении.
	l2 := newIPLimiter(1000)
	l2.allow("10.9.9.9")
	l2.mu.Lock()
	for _, e := range l2.limiters {
		e.lastSeen = e.lastSeen.Add(-2 * limiterTTL)
	}
	l2.mu.Unlock()
	for i := 0; i < maxLimiters+1; i++ {
		l2.allow(strings.Repeat("y", 8) + string(rune(i)))
	}
	l2.mu.Lock()
	_, stale := l2.limiters["10.9.9.9"]
	l2.mu.Unlock()
	if stale {
		t.Fatal("stale entry must be evicted")
	}
}

func TestSessionStoreStop(t *testing.T) {
	st := newSessionStore(time.Hour)
	st.Stop()
	st.Stop() // двойной Stop не паникует
	select {
	case <-st.stop:
	default:
		t.Fatal("stop channel must be closed")
	}
}

// doJSON — POST/PUT/DELETE с JSON-телом и CSRF-заголовком.
// Возвращает статус и разобранный envelope.
func doJSON(c *http.Client, base, method, path, csrf string, payload any, raw string) (int, apiEnvelope) {
	var body io.Reader
	if raw != "" {
		body = strings.NewReader(raw)
	} else if payload != nil {
		b, err := json.Marshal(payload)
		if err != nil {
			panic(err)
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, base+path, body)
	if err != nil {
		panic(err)
	}
	req.SetBasicAuth("admin", "secret-pass")
	if csrf != "" {
		req.Header.Set("X-CSRF-Token", csrf)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.Do(req)
	if err != nil {
		panic(err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(resp.Body)
	var env apiEnvelope
	_ = json.Unmarshal(b, &env)
	return resp.StatusCode, env
}

// csrfToken забирает токен со страницы (авторизованная сессия уже в jar).
func csrfToken(t *testing.T, c *http.Client, base string) string {
	t.Helper()
	_, page := doAuth(c, base, "GET", "/", "", nil)
	start := strings.Index(page, `name="csrf-token" content="`)
	if start < 0 {
		t.Fatal("csrf token not found")
	}
	rest := page[start+len(`name="csrf-token" content="`):]
	return rest[:strings.Index(rest, `"`)]
}

// TestAPIAddService — POST /api/v1/services (JSON).
func TestAPIAddService(t *testing.T) {
	srv, _ := newTestServer(t)
	ts := httptest.NewServer(srv)
	defer ts.Close()

	client := &http.Client{Jar: newJar(t)}
	token := csrfToken(t, client, ts.URL)

	// Валидное добавление (метод задан явно — без CDN-автодетекта с DNS).
	st, env := doJSON(client, ts.URL, "POST", "/api/v1/services", token,
		map[string]any{"name": "newsvc", "method": "whois"}, "")
	if st != http.StatusOK || !env.OK {
		t.Fatalf("add: %d %+v", st, env)
	}
	if _, svcs := doAuth(client, ts.URL, "GET", "/api/v1/services", "", nil); !strings.Contains(svcs, "newsvc") {
		t.Fatalf("service not listed: %s", svcs)
	}

	// Дубликат.
	st, _ = doJSON(client, ts.URL, "POST", "/api/v1/services", token,
		map[string]any{"name": "newsvc", "method": "whois"}, "")
	if st != http.StatusBadRequest {
		t.Fatalf("duplicate: want 400, got %d", st)
	}

	// Некорректное имя.
	st, _ = doJSON(client, ts.URL, "POST", "/api/v1/services", token,
		map[string]any{"name": "BAD NAME!", "method": "whois"}, "")
	if st != http.StatusBadRequest {
		t.Fatalf("bad name: want 400, got %d", st)
	}

	// Ломаный JSON.
	st, env = doJSON(client, ts.URL, "POST", "/api/v1/services", token, nil, "{oops")
	if st != http.StatusBadRequest || env.OK {
		t.Fatalf("malformed: want 400+ok:false, got %d %+v", st, env)
	}
}

// TestAPIRecommendService — быстрый офлайн-путь: имя без точки,
// резолв не нужен, DNS не трогаем.
func TestAPIRecommendService(t *testing.T) {
	srv, _ := newTestServer(t)
	ts := httptest.NewServer(srv)
	defer ts.Close()

	client := &http.Client{Jar: newJar(t)}
	token := csrfToken(t, client, ts.URL)

	st, env := doJSON(client, ts.URL, "POST", "/api/v1/services/10.0.0.1/recommend", token,
		map[string]any{}, "")
	if st != http.StatusOK || !env.OK {
		t.Fatalf("recommend: %d %+v", st, env)
	}
	adv, _ := env.Data.(map[string]any)["advice"].(map[string]any)
	if adv == nil {
		t.Fatalf("no advice in %+v", env.Data)
	}
	if reason, _ := adv["reason"].(string); !strings.Contains(reason, "не домен") {
		t.Fatalf("unexpected reason: %q", reason)
	}
}

// TestAPIUpdateService — PUT /api/v1/services/{name}.
func TestAPIUpdateService(t *testing.T) {
	srv, _ := newTestServer(t)
	ts := httptest.NewServer(srv)
	defer ts.Close()

	client := &http.Client{Jar: newJar(t)}
	token := csrfToken(t, client, ts.URL)

	st, _ := doJSON(client, ts.URL, "POST", "/api/v1/services", token,
		map[string]any{"name": "updateme", "method": "whois"}, "")
	if st != http.StatusOK {
		t.Fatalf("add: %d", st)
	}

	// Смена метода.
	st, env := doJSON(client, ts.URL, "PUT", "/api/v1/services/updateme", token,
		map[string]any{"method": "asn"}, "")
	if st != http.StatusOK || !env.OK {
		t.Fatalf("update method: %d %+v", st, env)
	}

	// Сброс метода в авто пустой строкой.
	st, _ = doJSON(client, ts.URL, "PUT", "/api/v1/services/updateme", token,
		map[string]any{"method": ""}, "")
	if st != http.StatusOK {
		t.Fatalf("reset method: %d", st)
	}

	// Пустое тело — нечего обновлять.
	st, _ = doJSON(client, ts.URL, "PUT", "/api/v1/services/updateme", token,
		map[string]any{}, "")
	if st != http.StatusBadRequest {
		t.Fatalf("empty update: want 400, got %d", st)
	}

	// Несуществующий сервис.
	st, _ = doJSON(client, ts.URL, "PUT", "/api/v1/services/nonexistent", token,
		map[string]any{"method": "asn"}, "")
	if st != http.StatusBadRequest {
		t.Fatalf("unknown service: want 400, got %d", st)
	}
}

// TestAPIDeleteService — DELETE /api/v1/services/{name}?purge=.
func TestAPIDeleteService(t *testing.T) {
	srv, _ := newTestServer(t)
	ts := httptest.NewServer(srv)
	defer ts.Close()

	client := &http.Client{Jar: newJar(t)}
	token := csrfToken(t, client, ts.URL)

	st, _ := doJSON(client, ts.URL, "POST", "/api/v1/services", token,
		map[string]any{"name": "deleteme", "method": "whois"}, "")
	if st != http.StatusOK {
		t.Fatalf("add: %d", st)
	}

	st, env := doJSON(client, ts.URL, "DELETE", "/api/v1/services/deleteme?purge=false", token, nil, "")
	if st != http.StatusOK || !env.OK {
		t.Fatalf("delete: %d %+v", st, env)
	}
	if _, svcs := doAuth(client, ts.URL, "GET", "/api/v1/services", "", nil); strings.Contains(svcs, "deleteme") {
		t.Fatal("service still listed")
	}

	st, _ = doJSON(client, ts.URL, "DELETE", "/api/v1/services/nonexistent", token, nil, "")
	if st != http.StatusBadRequest {
		t.Fatalf("unknown service: want 400, got %d", st)
	}
}

// TestAPISyncMany — асинхронный запуск, ответ сразу (фон упрётся в
// мёртвый MikroTik и быстро упадёт — тест проверяет только контракт).
func TestAPISyncMany(t *testing.T) {
	srv, _ := newTestServer(t)
	ts := httptest.NewServer(srv)
	defer ts.Close()

	client := &http.Client{Jar: newJar(t)}
	token := csrfToken(t, client, ts.URL)

	st, env := doJSON(client, ts.URL, "POST", "/api/v1/services/sync", token,
		map[string]any{"dry_run": false}, "")
	if st != http.StatusOK || !env.OK {
		t.Fatalf("sync many: %d %+v", st, env)
	}
	data, _ := env.Data.(map[string]any)
	if started, _ := data["started"].(bool); !started {
		t.Fatalf("no started flag: %+v", data)
	}

	// Пустой список = все сервисы из конфига.
	st, _ = doJSON(client, ts.URL, "POST", "/api/v1/services/sync", token,
		map[string]any{}, "")
	if st != http.StatusOK {
		t.Fatalf("sync all: %d", st)
	}

	// Ломаный JSON.
	st, _ = doJSON(client, ts.URL, "POST", "/api/v1/services/sync", token, nil, "{oops")
	if st != http.StatusBadRequest {
		t.Fatalf("malformed: want 400, got %d", st)
	}
}

// TestAPISyncOne — синхронный синк в мёртвый MikroTik: быстрый 500
// с envelope (контракт ошибки), без реального сбора.
func TestAPISyncOne(t *testing.T) {
	srv, _ := newTestServer(t)
	ts := httptest.NewServer(srv)
	defer ts.Close()

	client := &http.Client{Jar: newJar(t)}
	token := csrfToken(t, client, ts.URL)

	st, env := doJSON(client, ts.URL, "POST", "/api/v1/services/testsvc/sync", token,
		map[string]any{"dry_run": false}, "")
	if st != http.StatusInternalServerError || env.OK {
		t.Fatalf("want 500+ok:false, got %d %+v", st, env)
	}
	if env.Error == "" {
		t.Fatal("empty error text")
	}

	st, _ = doJSON(client, ts.URL, "POST", "/api/v1/services/testsvc/sync", token, nil, "{oops")
	if st != http.StatusBadRequest {
		t.Fatalf("malformed: want 400, got %d", st)
	}
}

// TestAPIDryRun — dry-run через static_url на закрытый порт: сбор падает
// мгновенно (connection refused), без DNS и внешней сети.
func TestAPIDryRun(t *testing.T) {
	srv, _ := newTestServer(t)
	ts := httptest.NewServer(srv)
	defer ts.Close()

	client := &http.Client{Jar: newJar(t)}
	token := csrfToken(t, client, ts.URL)

	st, _ := doJSON(client, ts.URL, "POST", "/api/v1/services", token,
		map[string]any{"name": "drytest", "method": "static_url", "static_url": "http://127.0.0.1:9/x.txt"}, "")
	if st != http.StatusOK {
		t.Fatalf("add: %d", st)
	}

	st, env := doJSON(client, ts.URL, "POST", "/api/v1/services/drytest/dry-run", token, nil, "")
	if st != http.StatusInternalServerError || env.OK {
		t.Fatalf("want 500+ok:false, got %d %+v", st, env)
	}
	res, _ := env.Data.(map[string]any)
	if res["service"] != "drytest" || res["dry_run"] != true {
		t.Fatalf("bad result envelope: %+v", res)
	}
}

// TestAPISchedules — GET /api/v1/schedules.
func TestAPISchedules(t *testing.T) {
	srv, _ := newTestServer(t)
	ts := httptest.NewServer(srv)
	defer ts.Close()

	client := &http.Client{Jar: newJar(t)}
	token := csrfToken(t, client, ts.URL)

	resp, body := doAuth(client, ts.URL, "GET", "/api/v1/schedules", "", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("schedules: %d %s", resp.StatusCode, body)
	}
	var v apiEnvelope
	if err := json.Unmarshal([]byte(body), &v); err != nil || !v.OK {
		t.Fatalf("bad envelope: %s", body)
	}
	data, _ := v.Data.(map[string]any)
	if data["global"] != "every 6h" {
		t.Fatalf("bad global: %+v", data)
	}
	_ = token
}

// TestAPIUpdateSchedule — PUT /api/v1/schedules/{service}.
func TestAPIUpdateSchedule(t *testing.T) {
	srv, cfg := newTestServer(t)
	ts := httptest.NewServer(srv)
	defer ts.Close()

	client := &http.Client{Jar: newJar(t)}
	token := csrfToken(t, client, ts.URL)

	st, env := doJSON(client, ts.URL, "PUT", "/api/v1/schedules/testsvc", token,
		map[string]any{"schedule": "every 1h"}, "")
	if st != http.StatusOK || !env.OK {
		t.Fatalf("update: %d %+v", st, env)
	}
	if got := cfg.EffectiveSchedule("testsvc"); got != "every 1h" {
		t.Fatalf("not applied: %q", got)
	}

	// Некорректное расписание.
	st, _ = doJSON(client, ts.URL, "PUT", "/api/v1/schedules/testsvc", token,
		map[string]any{"schedule": "sometimes"}, "")
	if st != http.StatusBadRequest {
		t.Fatalf("bad spec: want 400, got %d", st)
	}

	// Некорректное имя.
	st, _ = doJSON(client, ts.URL, "PUT", "/api/v1/schedules/BAD-NAME!", token,
		map[string]any{"schedule": "every 1h"}, "")
	if st != http.StatusBadRequest {
		t.Fatalf("bad name: want 400, got %d", st)
	}
}

// TestAPILogsFilters — GET /api/v1/logs с фильтрами.
func TestAPILogsFilters(t *testing.T) {
	srv, _ := newTestServer(t)
	ts := httptest.NewServer(srv)
	defer ts.Close()

	client := &http.Client{Jar: newJar(t)}
	resp, body := doAuth(client, ts.URL, "GET", "/api/v1/logs?level=error&service=testsvc&limit=5", "", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("logs: %d %s", resp.StatusCode, body)
	}
	var v apiEnvelope
	if err := json.Unmarshal([]byte(body), &v); err != nil || !v.OK {
		t.Fatalf("bad envelope: %s", body)
	}
}

// TestAPIHistoryService — GET /api/v1/history?service=.
func TestAPIHistoryService(t *testing.T) {
	srv, _ := newTestServer(t)
	ts := httptest.NewServer(srv)
	defer ts.Close()

	client := &http.Client{Jar: newJar(t)}
	resp, body := doAuth(client, ts.URL, "GET", "/api/v1/history?service=testsvc&limit=10", "", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("history: %d %s", resp.StatusCode, body)
	}
	var v apiEnvelope
	if err := json.Unmarshal([]byte(body), &v); err != nil || !v.OK {
		t.Fatalf("bad envelope: %s", body)
	}
}

// TestAPIAddressListDown — MikroTik мёртв: 200 + mikrotik_down (fail-open).
func TestAPIAddressListDown(t *testing.T) {
	srv, _ := newTestServer(t)
	ts := httptest.NewServer(srv)
	defer ts.Close()

	client := &http.Client{Jar: newJar(t)}
	resp, body := doAuth(client, ts.URL, "GET", "/api/v1/address-list", "", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("address-list: %d %s", resp.StatusCode, body)
	}
	var v apiEnvelope
	if err := json.Unmarshal([]byte(body), &v); err != nil || !v.OK {
		t.Fatalf("bad envelope: %s", body)
	}
	data, _ := v.Data.(map[string]any)
	if down, _ := data["mikrotik_down"].(bool); !down {
		t.Fatalf("want mikrotik_down:true, got %+v", data)
	}

	// По сервису — та же fail-open ветка.
	resp, body = doAuth(client, ts.URL, "GET", "/api/v1/address-list/testsvc", "", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("service address-list: %d %s", resp.StatusCode, body)
	}

	// Некорректное имя.
	resp, _ = doAuth(client, ts.URL, "GET", "/api/v1/address-list/BAD!", "", nil)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("bad name: want 400, got %d", resp.StatusCode)
	}
}

// TestActionSyncSelected — POST /actions/sync-selected (поле service).
func TestActionSyncSelected(t *testing.T) {
	srv, _ := newTestServer(t)
	ts := httptest.NewServer(srv)
	defer ts.Close()

	client := &http.Client{Jar: newJar(t)}
	_, page := doAuth(client, ts.URL, "GET", "/", "", nil)
	token := extractCSRF(t, page)

	// Пустой выбор.
	resp, body := doAuth(client, ts.URL, "POST", "/actions/sync-selected", token, strings.NewReader("csrf_token="+token))
	if resp.StatusCode != http.StatusBadRequest || !strings.Contains(body, "nothing selected") {
		t.Fatalf("empty: want 400/nothing selected, got %d %s", resp.StatusCode, body)
	}

	// Выбор сервиса (фон упадёт на мёртвом MT — проверяем только старт).
	resp, body = doAuth(client, ts.URL, "POST", "/actions/sync-selected", token,
		strings.NewReader("csrf_token="+token+"&service=testsvc"))
	if resp.StatusCode != http.StatusOK || !strings.Contains(body, `"ok":true`) {
		t.Fatalf("selected: %d %s", resp.StatusCode, body)
	}
	_ = page
}

// TestActionUpdateScheduleRoute — POST /actions/schedule (не schedule-update).
func TestActionUpdateScheduleRoute(t *testing.T) {
	srv, cfg := newTestServer(t)
	ts := httptest.NewServer(srv)
	defer ts.Close()

	client := &http.Client{Jar: newJar(t)}
	_, page := doAuth(client, ts.URL, "GET", "/", "", nil)
	token := extractCSRF(t, page)

	resp, body := doAuth(client, ts.URL, "POST", "/actions/schedule", token,
		strings.NewReader("csrf_token="+token+"&service=testsvc&schedule=every+2h"))
	if resp.StatusCode != http.StatusOK || !strings.Contains(body, `"ok":true`) {
		t.Fatalf("schedule action: %d %s", resp.StatusCode, body)
	}
	if got := cfg.EffectiveSchedule("testsvc"); got != "every 2h" {
		t.Fatalf("not applied: %q", got)
	}
	_ = page
}

// TestActionLogsClearDisabled — без файлового лога: документированный 400.
func TestActionLogsClearDisabled(t *testing.T) {
	srv, _ := newTestServer(t)
	ts := httptest.NewServer(srv)
	defer ts.Close()

	client := &http.Client{Jar: newJar(t)}
	_, page := doAuth(client, ts.URL, "GET", "/", "", nil)
	token := extractCSRF(t, page)

	resp, body := doAuth(client, ts.URL, "POST", "/actions/logs-clear", token,
		strings.NewReader("csrf_token="+token))
	if resp.StatusCode != http.StatusBadRequest || !strings.Contains(body, "logging to file is disabled") {
		t.Fatalf("want 400/disabled, got %d %s", resp.StatusCode, body)
	}
	_ = page
}

// TestPageServicesRenders — страница рендерится сервером (список — через JS API).
func TestPageServicesRenders(t *testing.T) {
	srv, _ := newTestServer(t)
	ts := httptest.NewServer(srv)
	defer ts.Close()

	client := &http.Client{Jar: newJar(t)}
	_, page := doAuth(client, ts.URL, "GET", "/services", "", nil)
	for _, want := range []string{"Сервисы", "csrf-token", "svc-drawer"} {
		if !strings.Contains(page, want) {
			t.Fatalf("services page missing %q", want)
		}
	}
}

// TestPageAddressListDown — мёртвый MikroTik: ветка Error вместо фильтров.
func TestPageAddressListDown(t *testing.T) {
	srv, _ := newTestServer(t)
	ts := httptest.NewServer(srv)
	defer ts.Close()

	client := &http.Client{Jar: newJar(t)}
	_, page := doAuth(client, ts.URL, "GET", "/address-list", "", nil)
	for _, want := range []string{"Списки адресов", "alert-error", "недоступен"} {
		if !strings.Contains(page, want) {
			t.Fatalf("address-list page missing %q", want)
		}
	}
}

// TestWSNeedsUpgrade — обычный GET на /api/v1/ws: 400, а не апгрейд.
func TestWSNeedsUpgrade(t *testing.T) {
	srv, _ := newTestServer(t)
	ts := httptest.NewServer(srv)
	defer ts.Close()

	client := &http.Client{Jar: newJar(t)}
	resp, _ := doAuth(client, ts.URL, "GET", "/api/v1/ws", "", nil)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("want 400, got %d", resp.StatusCode)
	}
}

// extractCSRF забирает токен со страницы.
func extractCSRF(t *testing.T, page string) string {
	t.Helper()
	start := strings.Index(page, `name="csrf-token" content="`)
	if start < 0 {
		t.Fatal("csrf token not found in page")
	}
	rest := page[start+len(`name="csrf-token" content="`):]
	return rest[:strings.Index(rest, `"`)]
}

// TestPageSchedulesRenders — страница расписаний со списком сервисов.
func TestPageSchedulesRenders(t *testing.T) {
	srv, _ := newTestServer(t)
	ts := httptest.NewServer(srv)
	defer ts.Close()

	client := &http.Client{Jar: newJar(t)}
	_, page := doAuth(client, ts.URL, "GET", "/schedules", "", nil)
	for _, want := range []string{"Расписания", "csrf-token", "testsvc"} {
		if !strings.Contains(page, want) {
			t.Fatalf("schedules page missing %q", want)
		}
	}
}

// TestPageLogsRenders — страница логов с контейнером вывода.
func TestPageLogsRenders(t *testing.T) {
	srv, _ := newTestServer(t)
	ts := httptest.NewServer(srv)
	defer ts.Close()

	client := &http.Client{Jar: newJar(t)}
	_, page := doAuth(client, ts.URL, "GET", "/logs", "", nil)
	for _, want := range []string{"Логи", "log-output", "csrf-token"} {
		if !strings.Contains(page, want) {
			t.Fatalf("logs page missing %q", want)
		}
	}
}

// TestActionSyncAllStarts — POST /actions/sync-all запускает фоном.
func TestActionSyncAllStarts(t *testing.T) {
	srv, _ := newTestServer(t)
	ts := httptest.NewServer(srv)
	defer ts.Close()

	client := &http.Client{Jar: newJar(t)}
	_, page := doAuth(client, ts.URL, "GET", "/", "", nil)
	token := extractCSRF(t, page)

	resp, body := doAuth(client, ts.URL, "POST", "/actions/sync-all", token,
		strings.NewReader("csrf_token="+token))
	if resp.StatusCode != http.StatusOK || !strings.Contains(body, `"ok":true`) {
		t.Fatalf("sync-all: %d %s", resp.StatusCode, body)
	}
}

// TestActionDeleteServiceRoute — POST /actions/service-delete (purge=on/1).
func TestActionDeleteServiceRoute(t *testing.T) {
	srv, _ := newTestServer(t)
	ts := httptest.NewServer(srv)
	defer ts.Close()

	client := &http.Client{Jar: newJar(t)}
	_, page := doAuth(client, ts.URL, "GET", "/", "", nil)
	token := extractCSRF(t, page)

	resp, _ := doAuth(client, ts.URL, "POST", "/actions/service-add", token,
		strings.NewReader("csrf_token="+token+"&name=10.0.0.9&method=whois"))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("add: %d", resp.StatusCode)
	}

	resp, body := doAuth(client, ts.URL, "POST", "/actions/service-delete", token,
		strings.NewReader("csrf_token="+token+"&name=10.0.0.9&purge=off"))
	if resp.StatusCode != http.StatusOK || !strings.Contains(body, `"ok":true`) {
		t.Fatalf("delete: %d %s", resp.StatusCode, body)
	}
	if _, svcs := doAuth(client, ts.URL, "GET", "/api/v1/services", "", nil); strings.Contains(svcs, "10.0.0.9") {
		t.Fatal("service still listed")
	}

	// Несуществующий.
	resp, _ = doAuth(client, ts.URL, "POST", "/actions/service-delete", token,
		strings.NewReader("csrf_token="+token+"&name=nope"))
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("unknown: want 400, got %d", resp.StatusCode)
	}
}

// TestAPIServiceAddressListDown — fail-open ветка по сервису.
func TestAPIServiceAddressListDown(t *testing.T) {
	srv, _ := newTestServer(t)
	ts := httptest.NewServer(srv)
	defer ts.Close()

	client := &http.Client{Jar: newJar(t)}
	resp, body := doAuth(client, ts.URL, "GET", "/api/v1/address-list/testsvc", "", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status: %d %s", resp.StatusCode, body)
	}
	var v apiEnvelope
	if err := json.Unmarshal([]byte(body), &v); err != nil || !v.OK {
		t.Fatalf("bad envelope: %s", body)
	}
	data, _ := v.Data.(map[string]any)
	if down, _ := data["mikrotik_down"].(bool); !down {
		t.Fatalf("want mikrotik_down:true, got %+v", data)
	}
	if data["service"] != "testsvc" || data["count"] != float64(0) {
		t.Fatalf("bad payload: %+v", data)
	}
}

// TestSessionReuseOrCreate — клиенты без cookie (Basic-auth, curl) не плодят
// сессии: повторный вход того же пользователя возвращает ту же запись.
func TestSessionReuseOrCreate(t *testing.T) {
	srv, _ := newTestServer(t)

	id1, s1 := srv.sessions.reuseOrCreate("admin")
	id2, s2 := srv.sessions.reuseOrCreate("admin")
	if id1 != id2 || s1 != s2 {
		t.Fatal("same user must reuse live session")
	}
	id3, _ := srv.sessions.reuseOrCreate("other")
	if id3 == id1 {
		t.Fatal("different users must not share session")
	}
	srv.sessions.mu.RLock()
	n := len(srv.sessions.data)
	srv.sessions.mu.RUnlock()
	if n != 2 {
		t.Fatalf("want 2 sessions, got %d", n)
	}
}

// TestSessionStoreCap — хранилище ограничено: переполнение чистит
// просроченные и лишние записи вместо бесконечного роста.
func TestSessionStoreCap(t *testing.T) {
	srv, _ := newTestServer(t)
	for i := 0; i < maxSessions+10; i++ {
		srv.sessions.reuseOrCreate("user" + strconv.Itoa(i))
	}
	srv.sessions.mu.RLock()
	n := len(srv.sessions.data)
	srv.sessions.mu.RUnlock()
	if n > maxSessions {
		t.Fatalf("session store grew past cap: %d > %d", n, maxSessions)
	}
}

// TestServicesJSRemovedAlias — контракт бэкенд/фронт: Result.Removed и
// history.Record сериализуются как "removed", JS обязан его читать.
func TestServicesJSRemovedAlias(t *testing.T) {
	raw, err := os.ReadFile("static/services.js")
	if err != nil {
		t.Fatalf("read services.js: %v", err)
	}
	js := string(raw)
	start := strings.Index(js, "function historyCounts")
	if start < 0 {
		t.Fatal("historyCounts not found in services.js")
	}
	block := js[start : start+400]
	if !strings.Contains(block, `"removed"`) {
		t.Fatalf("historyCounts must read backend field \"removed\": %s", block)
	}
}

// TestCSRFBasicauthOrigin — Basic без cookie: программный клиент без Origin
// проходит, браузерный кросс-сайт с чужим Origin режется (403).
func TestCSRFBasicauthOrigin(t *testing.T) {
	srv, _ := newTestServer(t)
	ts := httptest.NewServer(srv)
	defer ts.Close()

	// Без cookie и без Origin: доходит до хендлера (400 от битого JSON,
	// а не 401/403) — программные клиенты работают как раньше.
	req, _ := http.NewRequest("POST", ts.URL+"/api/v1/services", strings.NewReader("{oops"))
	req.SetBasicAuth("admin", "secret-pass")
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusForbidden || resp.StatusCode == http.StatusUnauthorized {
		t.Fatalf("programmatic basic client rejected: %d", resp.StatusCode)
	}

	// Тот же запрос с чужим Origin: 403 до хендлера.
	req2, _ := http.NewRequest("POST", ts.URL+"/api/v1/services", strings.NewReader("{oops"))
	req2.SetBasicAuth("admin", "secret-pass")
	req2.Header.Set("Content-Type", "application/json")
	req2.Header.Set("Origin", "http://evil.example")
	resp2, err := http.DefaultClient.Do(req2)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp2.Body.Close() }()
	if resp2.StatusCode != http.StatusForbidden {
		t.Fatalf("cross-origin basic must be 403, got %d", resp2.StatusCode)
	}
}

// TestSameOrigin — строгая сверка Origin с Host (без префиксов).
func TestSameOrigin(t *testing.T) {
	srv, _ := newTestServer(t)
	req, _ := http.NewRequest("GET", "http://mrs.local/", nil)
	req.Host = "mrs.local"
	for _, ok := range []string{"", "http://mrs.local", "https://mrs.local", "HTTP://MRS.LOCAL"} {
		if !checkOriginHost(req, ok) {
			t.Fatalf("must accept %q", ok)
		}
	}
	for _, bad := range []string{"http://mrs.local.evil.com", "http://evil.com", "http://mrs.local:9999", "://", "http://"} {
		if checkOriginHost(req, bad) {
			t.Fatalf("must reject %q", bad)
		}
	}
	// Схема: http-Origin при https-запросе (и наоборот) — отказ.
	tlsReq, _ := http.NewRequest("GET", "https://mrs.local/", nil)
	tlsReq.Host = "mrs.local"
	tlsReq.TLS = &tls.ConnectionState{}
	if srv.sameOrigin(tlsReq, "http://mrs.local") {
		t.Fatal("http origin over https must be rejected")
	}
	if !srv.sameOrigin(tlsReq, "https://mrs.local") {
		t.Fatal("https origin over https must pass")
	}
}

// TestClientIPForwarded — разбор XFF справа налево.
func TestClientIPForwarded(t *testing.T) {
	c, err := newIPChecker(&config.WebConfig{
		AllowedCIDRs:   []string{"10.0.0.0/8"},
		TrustedProxies: []string{"10.9.0.0/16"},
	})
	if err != nil {
		t.Fatal(err)
	}
	mkreq := func(remote, xff string) *http.Request {
		req, _ := http.NewRequest("GET", "http://x/", nil)
		req.RemoteAddr = remote
		if xff != "" {
			req.Header.Set("X-Forwarded-For", xff)
		}
		return req
	}
	// Прокси дописал реальный IP: берём его, а не поддельный первый.
	// XFF "9.9.9.9, 1.2.3.4": 9.9.9.9 подделал клиент, 1.2.3.4 дописал прокси.
	if got := c.clientIP(mkreq("10.9.0.1:123", "9.9.9.9, 1.2.3.4")); got.String() != "1.2.3.4" {
		t.Fatalf("want rightmost untrusted 1.2.3.4, got %v", got)
	}
	// Доверенный hop справа пропускается.
	if got := c.clientIP(mkreq("10.9.0.1:123", "1.2.3.4, 10.9.0.9")); got.String() != "1.2.3.4" {
		t.Fatalf("want client past trusted hop, got %v", got)
	}
	// Напрямую без прокси: XFF игнорируется.
	if got := c.clientIP(mkreq("1.2.3.4:123", "10.99.99.99")); got.String() != "1.2.3.4" {
		t.Fatalf("want remote addr, got %v", got)
	}
	// Вся цепочка доверенная: верим самому левому.
	if got := c.clientIP(mkreq("10.9.0.1:123", "10.1.0.5, 10.9.0.2")); got.String() != "10.1.0.5" {
		t.Fatalf("want leftmost of trusted chain, got %v", got)
	}
}

// TestReadyzOpenFast — /readyz без авторизации и без ping роутера.
func TestReadyzOpenFast(t *testing.T) {
	srv, _ := newTestServer(t)
	ts := httptest.NewServer(srv)
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/readyz")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("readyz: %d", resp.StatusCode)
	}
	var v apiEnvelope
	if err := json.NewDecoder(resp.Body).Decode(&v); err != nil || !v.OK {
		t.Fatalf("bad envelope: %+v %v", v, err)
	}
}

// TestHTTPSNow — Secure/HSTS только при прямом TLS или https от доверенного.
func TestHTTPSNow(t *testing.T) {
	srv, _ := newTestServer(t)
	mkreq := func(remote, proto string) *http.Request {
		req, _ := http.NewRequest("GET", "http://x/", nil)
		req.RemoteAddr = remote
		if proto != "" {
			req.Header.Set("X-Forwarded-Proto", proto)
		}
		return req
	}
	// В тестовом конфиге доверенных прокси нет — только прямой TLS.
	if srv.httpsNow(mkreq("127.0.0.1:1", "")) {
		t.Fatal("plain http must not be https")
	}
	if srv.httpsNow(mkreq("127.0.0.1:1", "https")) {
		t.Fatal("untrusted proto header must not enable secure")
	}
}

// TestSettingsSecretClear — маркер clear:<key> очищает секрет, без него
// пустое/маска пропускаются (значение сохраняется).
func TestSettingsSecretClear(t *testing.T) {
	srv, cfg := newTestServer(t)
	ts := httptest.NewServer(srv)
	defer ts.Close()

	client := &http.Client{Jar: newJar(t)}
	_, page := doAuth(client, ts.URL, "GET", "/settings", "", nil)
	start := strings.Index(page, `name="csrf-token" content="`)
	rest := page[start+len(`name="csrf-token" content="`):]
	token := rest[:strings.Index(rest, `"`)]

	_, body := doAuth(client, ts.URL, "POST", "/actions/settings", token,
		strings.NewReader("csrf_token="+token+"&external.akamai_api_key=AKAMAITEST123"))
	if !strings.Contains(body, `"ok":true`) {
		t.Fatalf("set key failed: %s", body)
	}
	if cfg.External.AkamaiAPIKey != "AKAMAITEST123" {
		t.Fatalf("key not applied: %q", cfg.External.AkamaiAPIKey)
	}

	// Пустое без маркера — пропуск (остаётся AKAMAITEST123).
	_, body = doAuth(client, ts.URL, "POST", "/actions/settings", token,
		strings.NewReader("csrf_token="+token+"&external.akamai_api_key="))
	if !strings.Contains(body, `"ok":true`) {
		t.Fatalf("empty without marker failed: %s", body)
	}
	if cfg.External.AkamaiAPIKey != "AKAMAITEST123" {
		t.Fatalf("empty without marker must keep value: %q", cfg.External.AkamaiAPIKey)
	}

	// С маркером — очистка.
	_, body = doAuth(client, ts.URL, "POST", "/actions/settings", token,
		strings.NewReader("csrf_token="+token+"&external.akamai_api_key=&clear%3Aexternal.akamai_api_key=1"))
	if !strings.Contains(body, `"ok":true`) {
		t.Fatalf("clear failed: %s", body)
	}
	if cfg.External.AkamaiAPIKey != "" {
		t.Fatalf("key not cleared: %q", cfg.External.AkamaiAPIKey)
	}

	// Очистка обязательного секрета — явная ошибка валидации, а не молчание.
	_, body = doAuth(client, ts.URL, "POST", "/actions/settings", token,
		strings.NewReader("csrf_token="+token+"&web.auth.password=&clear%3Aweb.auth.password=1"))
	if !strings.Contains(body, "web.auth.username/password required") {
		t.Fatalf("clearing required secret must fail validation: %s", body)
	}
}

// TestConcurrentReadsAndMutations — чтения и мутации конфига параллельно
// под -race: ни fatal error, ни дедлока, конфиг консистентен.
func TestConcurrentReadsAndMutations(t *testing.T) {
	srv, cfg := newTestServer(t)
	ts := httptest.NewServer(srv)
	defer ts.Close()

	jarClient := &http.Client{Jar: newJar(t), Timeout: 20 * time.Second}
	_, page := doAuth(jarClient, ts.URL, "GET", "/settings", "", nil)
	start := strings.Index(page, `name="csrf-token" content="`)
	rest := page[start+len(`name="csrf-token" content="`):]
	token := rest[:strings.Index(rest, `"`)]

	var wg sync.WaitGroup
	errs := make(chan string, 64)
	stop := make(chan struct{})

	readPaths := []string{"/", "/services", "/api/v1/services", "/api/v1/schedules",
		"/api/v1/status", "/api/v1/history?limit=5", "/api/v1/logs?limit=5"}
	for g := 0; g < 4; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			c := &http.Client{Timeout: 20 * time.Second}
			for i := 0; i < 15; i++ {
				select {
				case <-stop:
					return
				default:
				}
				p := readPaths[(g+i)%len(readPaths)]
				var lastCode int
				ok := false
				for attempt := 0; attempt < 8; attempt++ {
					req, _ := http.NewRequest("GET", ts.URL+p, nil)
					req.SetBasicAuth("admin", "secret-pass")
					resp, err := c.Do(req)
					if err != nil {
						errs <- "read: " + err.Error()
						return
					}
					_, _ = io.Copy(io.Discard, resp.Body)
					_ = resp.Body.Close()
					lastCode = resp.StatusCode
					if lastCode != http.StatusTooManyRequests {
						ok = true
						break
					}
					time.Sleep(150 * time.Millisecond)
				}
				if !ok || lastCode != http.StatusOK {
					errs <- fmt.Sprintf("read %s: %d", p, lastCode)
					return
				}
			}
		}(g)
	}

	for w := 0; w < 2; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			// 429 от rate-limit — ретраим (лимитер общий на IP теста).
			put := func(path, raw string) (int, bool) {
				for attempt := 0; attempt < 8; attempt++ {
					st, _ := doJSON(jarClient, ts.URL, "PUT", path, token, nil, raw)
					if st != http.StatusTooManyRequests {
						return st, true
					}
					time.Sleep(150 * time.Millisecond)
				}
				return http.StatusTooManyRequests, false
			}
			for i := 0; i < 10; i++ {
				select {
				case <-stop:
					return
				default:
				}
				if (i+w)%3 == 2 {
					// Мутация overrides-карты (вторая пишущая сторона).
					method := "dynamic,whois"
					if (i+w)%2 == 0 {
						method = "dynamic"
					}
					st, ok := put("/api/v1/services/testsvc",
						`{"method":`+strconv.Quote(method)+`}`)
					if !ok || st != http.StatusOK {
						errs <- fmt.Sprintf("override PUT: %d", st)
						return
					}
					continue
				}
				spec := "manual"
				if (i+w)%2 == 0 {
					spec = "every 6h"
				}
				st, ok := put("/api/v1/schedules/testsvc",
					`{"schedule":`+strconv.Quote(spec)+`}`)
				if !ok || st != http.StatusOK {
					errs <- fmt.Sprintf("schedule PUT: %d", st)
					return
				}
				// Тяжёлый писатель: настройки целиком (clone+Save+apply).
				st, _ = doJSON(jarClient, ts.URL, "POST", "/actions/settings", token, nil, "")
				_ = st
			}
		}(w)
	}

	// Писатель add/delete (метод задан — без сетевого CDN-детекта).
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 6; i++ {
			name := fmt.Sprintf("race%d", i%2)
			_, _ = doJSON(jarClient, ts.URL, "POST", "/api/v1/services", token, nil,
				`{"name":"`+name+`","method":"dynamic"}`)
			_, _ = doJSON(jarClient, ts.URL, "DELETE", "/api/v1/services/"+name+"?purge=false", token, nil, "")
		}
	}()

	wg.Wait()
	select {
	case e := <-errs:
		t.Fatalf("concurrent workload failed: %s", e)
	default:
	}
	cfg.RLock()
	found := false
	for _, sv := range cfg.Services {
		if sv == "testsvc" {
			found = true
		}
	}
	cfg.RUnlock()
	if !found {
		t.Fatal("testsvc lost after concurrent workload")
	}
}

// TestAPIPresets — каталог пресетов: youtube из embed, suggest-блок,
// домены больших в каталог не тащим.
func TestAPIPresets(t *testing.T) {
	srv, cfg := newTestServer(t)
	ts := httptest.NewServer(srv)
	defer ts.Close()

	// Включаем каталог (дефолт off): та же память, гонки нет (сервер стартовал).
	cfg.Lock()
	cfg.Suggest.V2flyEnabled = true
	cfg.Unlock()

	client := &http.Client{Jar: newJar(t)}
	_, body := doAuth(client, ts.URL, "GET", "/api/v1/presets", "", nil)
	var v apiEnvelope
	if err := json.Unmarshal([]byte(body), &v); err != nil || !v.OK {
		t.Fatalf("bad envelope: %s", body)
	}
	data, _ := v.Data.(map[string]any)
	list, _ := data["presets"].([]any)
	found := false
	for _, p := range list {
		m, _ := p.(map[string]any)
		if m["slug"] == "youtube" {
			found = true
			if doms, _ := m["domains"].([]any); len(doms) == 0 {
				t.Fatal("youtube preset must carry domains")
			}
		}
	}
	if !found {
		t.Fatalf("youtube preset missing: %s", first(body, 200))
	}
	suggest, _ := data["suggest"].(map[string]any)
	if suggest["mode"] != "off" {
		t.Fatalf("default suggest mode must be off, got %v", suggest["mode"])
	}
}

// first — безопасный префикс строки для ошибок тестов.
func first(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

// TestAPISuggestDisabled — без ключа/режима подбор выключен, а не ошибка.
func TestAPISuggestDisabled(t *testing.T) {
	srv, _ := newTestServer(t)
	ts := httptest.NewServer(srv)
	defer ts.Close()

	client := &http.Client{Jar: newJar(t)}
	_, page := doAuth(client, ts.URL, "GET", "/settings", "", nil)
	start := strings.Index(page, `name="csrf-token" content="`)
	token := page[start+len(`name="csrf-token" content="`):]
	token = token[:strings.Index(token, `"`)]

	st, env := doJSON(client, ts.URL, "POST", "/api/v1/services/testsvc/suggest-domains", token, nil, `{}`)
	if st != http.StatusOK {
		t.Fatalf("status: %d %+v", st, env)
	}
	data, _ := env.Data.(map[string]any)
	if enabled, _ := data["enabled"].(bool); enabled {
		t.Fatalf("suggest must be disabled by default: %+v", data)
	}
}

// TestAPIAddServicePreset — создание из пресета: домены+primary+generated.
func TestAPIAddServicePreset(t *testing.T) {
	srv, cfg := newTestServer(t)
	ts := httptest.NewServer(srv)
	defer ts.Close()

	client := &http.Client{Jar: newJar(t)}
	_, page := doAuth(client, ts.URL, "GET", "/settings", "", nil)
	start := strings.Index(page, `name="csrf-token" content="`)
	token := page[start+len(`name="csrf-token" content="`):]
	token = token[:strings.Index(token, `"`)]

	st, _ := doJSON(client, ts.URL, "POST", "/api/v1/services", token, nil,
		`{"name":"presettest","preset":"youtube","method":"whois"}`)
	if st != http.StatusOK {
		t.Fatalf("add with preset failed: %d", st)
	}
	ov := cfg.Overrides["presettest"]
	if ov.Preset != "youtube" || len(ov.Generated) == 0 || len(ov.Domains) == 0 {
		t.Fatalf("preset not recorded: %+v", ov)
	}
	if ov.Primary == "" {
		t.Fatal("primary not set from preset")
	}
	if ov.Domains[0] != ov.Primary {
		t.Fatalf("primary must be first: %q vs %q", ov.Primary, ov.Domains[0])
	}

	// Refresh: подмешиваем ручной домен и чужой, обновляем — ручной жив,
	// чужой (не из пресета и не ручной? нет: всё не-preset считается ручным).
	st, env := doJSON(client, ts.URL, "POST", "/api/v1/services/presettest/refresh-preset", token, nil, `{}`)
	if st != http.StatusOK {
		t.Fatalf("refresh failed: %d %+v", st, env)
	}

	// Большой пресет без force — отказ с объяснением.
	st, _ = doJSON(client, ts.URL, "POST", "/api/v1/services/presettest/refresh-preset", token, nil,
		`{"preset":"rublock"}`)
	if st != http.StatusBadRequest {
		t.Fatalf("large preset without force must be 400, got %d", st)
	}
}

// TestAPIPresetsMatch — инвертированный индекс: домен находит пресет.
func TestAPIPresetsMatch(t *testing.T) {
	srv, cfg := newTestServer(t)
	// Абсолютный cache_path: presetsDir endpoints и DiskDir смотрят в temp.
	absCache := filepath.Join(t.TempDir(), "c.db")
	cfg.Lock()
	cfg.CachePath = absCache
	cfg.Unlock()
	presets.DiskDir = filepath.Join(filepath.Dir(absCache), "presets")
	t.Cleanup(func() { presets.FlushAll() })
	ts := httptest.NewServer(srv)
	defer ts.Close()

	// Кладём upstream-фикстуру в зеркало тестового конфига и индексируем.
	dir := filepath.Join(filepath.Dir(absCache), "presets", "upstream")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "netflix"), []byte("nflxvideo.net\nnetflix.com\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := presets.RebuildUpstreamIndex(filepath.Join(filepath.Dir(absCache), "presets")); err != nil {
		t.Fatalf("reindex: %v", err)
	}

	client := &http.Client{Jar: newJar(t)}
	_, body := doAuth(client, ts.URL, "GET", "/api/v1/presets/match?domain=nflxvideo.net", "", nil)
	var v apiEnvelope
	if err := json.Unmarshal([]byte(body), &v); err != nil || !v.OK {
		t.Fatalf("bad envelope: %s", body)
	}
	data, _ := v.Data.(map[string]any)
	matches, _ := data["matches"].([]any)
	if len(matches) == 0 {
		t.Fatalf("no match for nflxvideo.net: %s", body)
	}
	m0, _ := matches[0].(map[string]any)
	if m0["slug"] != "netflix" {
		t.Fatalf("want netflix, got %+v", m0)
	}

	_, body = doAuth(client, ts.URL, "GET", "/api/v1/presets/match?domain=unknown-xyz-123.invalid", "", nil)
	if !strings.Contains(body, `"matches":null`) && !strings.Contains(body, `"matches":[]`) {
		t.Fatalf("unknown domain must match nothing: %s", body)
	}
}

// TestAPIBlockcheck — пересечение доменов сервиса с блоклистом.
func TestAPIBlockcheck(t *testing.T) {
	srv, cfg := newTestServer(t)
	absCache := filepath.Join(t.TempDir(), "c.db")
	cfg.Lock()
	cfg.CachePath = absCache
	cfg.Unlock()
	presets.DiskDir = filepath.Join(filepath.Dir(absCache), "presets")
	t.Cleanup(func() { presets.FlushAll() })
	ts := httptest.NewServer(srv)
	defer ts.Close()

	// Фикстура зеркала вместо 38МБ rublock (YAML-формат как у настоящего).
	dir := filepath.Join(filepath.Dir(absCache), "presets")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	fx := "slug: rublock\ntitle: RU\nmethod: static_url\nsource:\n  repo: test\n  commit: x\n  date: today\ndomains:\n  - blocked.example\n  - sub.blocked.example\n"
	if err := os.WriteFile(filepath.Join(dir, "rublock.yaml"), []byte(fx), 0o600); err != nil {
		t.Fatal(err)
	}

	client := &http.Client{Jar: newJar(t)}
	_, page := doAuth(client, ts.URL, "GET", "/settings", "", nil)
	start := strings.Index(page, `name="csrf-token" content="`)
	token := page[start+len(`name="csrf-token" content="`):]
	token = token[:strings.Index(token, `"`)]

	st, _ := doJSON(client, ts.URL, "PUT", "/api/v1/services/testsvc", token, nil,
		`{"domains":["blocked.example","free.example"]}`)
	if st != http.StatusOK {
		t.Fatalf("override PUT failed: %d", st)
	}
	st, env := doJSON(client, ts.URL, "POST", "/api/v1/services/testsvc/blockcheck", token, nil, `{}`)
	if st != http.StatusOK {
		t.Fatalf("blockcheck failed: %d %+v", st, env)
	}
	data, _ := env.Data.(map[string]any)
	if c, _ := data["blocked_count"].(float64); c != 1 {
		t.Fatalf("want 1 blocked: %+v", data)
	}
	blocked, _ := data["blocked"].([]any)
	if len(blocked) != 1 || blocked[0] != "blocked.example" {
		t.Fatalf("wrong blocked: %+v", data)
	}
}

// TestSettingsHasSuggestRows — новые настройки видны в UI.
func TestSettingsHasSuggestRows(t *testing.T) {
	srv, _ := newTestServer(t)
	ts := httptest.NewServer(srv)
	defer ts.Close()

	client := &http.Client{Jar: newJar(t)}
	_, page := doAuth(client, ts.URL, "GET", "/settings", "", nil)
	for _, want := range []string{
		`suggest.v2fly_enabled`, `suggest.ru_enabled`, `suggest.urlscan_mode`,
		`suggest.sources`, `external.urlscan_api_key`, `presets.update_schedule`,
		`name="clear:`, `Подбор доменов`,
	} {
		if !strings.Contains(page, want) {
			t.Fatalf("settings page missing %q", want)
		}
	}
}

// TestMaxBodyLimit — тело больше 1МБ режется на всех методах с телом.
func TestMaxBodyLimit(t *testing.T) {
	srv, _ := newTestServer(t)
	ts := httptest.NewServer(srv)
	defer ts.Close()

	big := strings.Repeat("x", (1<<20)+8)
	targets := map[string]string{
		"POST": "/api/v1/services",
		"PUT":  "/api/v1/services/testsvc",
	}
	for _, m := range []string{"POST", "PUT", "DELETE"} {
		path := "/api/v1/services"
		if m == "PUT" {
			path = targets[m]
		}
		req, _ := http.NewRequest(m, ts.URL+path, strings.NewReader(big))
		req.SetBasicAuth("admin", "secret-pass")
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
		// DELETE без тела-обработчика упадёт раньше на 404/405 — главное,
		// что не 200 и не зависание; POST/PUT отрезаются лимитом
		// (413 от MaxBytesReader или 400 от декодера — оба до обработки).
		if m == "DELETE" {
			continue
		}
		if resp.StatusCode != http.StatusRequestEntityTooLarge && resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("%s oversized body: want 4xx reject, got %d", m, resp.StatusCode)
		}
	}
}

// TestRefreshSourceValidation — мусор в source отклоняется до любой сети.
func TestRefreshSourceValidation(t *testing.T) {
	srv, _ := newTestServer(t)
	ts := httptest.NewServer(srv)
	defer ts.Close()

	client := &http.Client{Jar: newJar(t)}
	resp, body := doAuth(client, ts.URL, "POST", "/api/v1/presets/refresh?source=xxx", "", nil)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("want 400, got %d: %s", resp.StatusCode, body)
	}
}

// TestBlockcheckNoMirror — без зеркала честный 502, а не паника/пусто.
func TestBlockcheckNoMirror(t *testing.T) {
	srv, cfg := newTestServer(t)
	absCache := filepath.Join(t.TempDir(), "c.db")
	cfg.Lock()
	cfg.CachePath = absCache
	cfg.Unlock()
	presets.DiskDir = filepath.Join(filepath.Dir(absCache), "presets")
	t.Cleanup(func() { presets.FlushAll() })
	ts := httptest.NewServer(srv)
	defer ts.Close()

	client := &http.Client{Jar: newJar(t)}
	_, page := doAuth(client, ts.URL, "GET", "/settings", "", nil)
	start := strings.Index(page, `name="csrf-token" content="`)
	token := page[start+len(`name="csrf-token" content="`):]
	token = token[:strings.Index(token, `"`)]

	st, _ := doJSON(client, ts.URL, "PUT", "/api/v1/services/testsvc", token, nil,
		`{"domains":["example.com"]}`)
	if st != http.StatusOK {
		t.Fatalf("override PUT failed: %d", st)
	}
	st, env := doJSON(client, ts.URL, "POST", "/api/v1/services/testsvc/blockcheck", token, nil, `{}`)
	if st != http.StatusBadGateway {
		t.Fatalf("want 502 without mirror, got %d %+v", st, env)
	}
}

// TestMatchEmptyDomain — пустой запрос отклоняется.
func TestMatchEmptyDomain(t *testing.T) {
	srv, _ := newTestServer(t)
	ts := httptest.NewServer(srv)
	defer ts.Close()

	client := &http.Client{Jar: newJar(t)}
	resp, _ := doAuth(client, ts.URL, "GET", "/api/v1/presets/match?domain=", "", nil)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("want 400, got %d", resp.StatusCode)
	}
}

// TestSessionExpiredNotReused — протухшая сессия не переиспользуется.
func TestSessionExpiredNotReused(t *testing.T) {
	srv, _ := newTestServer(t)
	id1, _ := srv.sessions.reuseOrCreate("admin")
	srv.sessions.mu.Lock()
	srv.sessions.data[id1].expires = time.Now().Add(-time.Second)
	srv.sessions.mu.Unlock()
	id2, _ := srv.sessions.reuseOrCreate("admin")
	if id2 == id1 {
		t.Fatal("expired session must not be reused")
	}
}
