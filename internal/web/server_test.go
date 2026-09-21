package web

import (
	"encoding/json"
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
  gateway: gw
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
	t.Cleanup(func() { cache.Close() })

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	s, err := core.NewSyncer(cfg, log, cache, &notifier.NoopNotifier{})
	if err != nil {
		t.Fatal(err)
	}
	srv, err := NewServer(cfg, s, log)
	if err != nil {
		t.Fatal(err)
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
	defer resp.Body.Close()
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
	defer resp.Body.Close()
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
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", resp.StatusCode)
	}

	client := &http.Client{Jar: newJar(t)}
	resp, page := doAuth(client, ts.URL, "GET", "/", "", nil)
	if resp.StatusCode != 200 {
		t.Fatalf("dashboard: %d body=%s", resp.StatusCode, page)
	}
	if !strings.Contains(page, "MikroTik Route Sync") || !strings.Contains(page, "testsvc") {
		t.Fatalf("dashboard content wrong: %s", page)
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
		strings.NewReader("name=newsvc&csrf_token="+token))
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
		`name="mikrotik.comment_prefix"`,
		`name="safety.require_confirmation_over"`,
		`name="retry.jitter"`,
		`data-help=`,
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
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("healthz must be allowed without cidr check, got %d", resp.StatusCode)
	}
	resp, err = http.Get(ts.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
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
