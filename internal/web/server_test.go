package web

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Kfaraon/mikrotik-route-sync/internal/config"
	"github.com/Kfaraon/mikrotik-route-sync/internal/core"
	"github.com/Kfaraon/mikrotik-route-sync/internal/notifier"
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

	client := &http.Client{Jar: newJar(t)}
	if resp, body := doAuth(client, ts.URL, "GET", "/", "", nil); resp.StatusCode != 200 {
		t.Fatalf("auth GET failed: %d %s", resp.StatusCode, body)
	}

	resp, _ := doAuth(client, ts.URL, "POST", "/actions/sync-all", "", strings.NewReader(""))
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("expected 403 without csrf, got %d", resp.StatusCode)
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
