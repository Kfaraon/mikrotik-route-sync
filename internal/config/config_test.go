package config

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"gopkg.in/yaml.v3"
)

func writeTempConfig(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

const minimalYAML = `
timezone: UTC
mikrotik:
  host: 192.168.88.1
  username: api
  password: secret
  use_ssl: true
  verify_ssl: true
firewall:
  address_list: TO-VPN
web:
  enabled: true
  auth:
    enabled: true
    username: admin
    password: adminpass
schedules:
  global: "every 6h"
services: [cloudflare]
`

func TestLoadDefaultsAndValidate(t *testing.T) {
	path := writeTempConfig(t, minimalYAML)
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.Safety.MinPrefixV4 != 8 {
		t.Fatalf("min prefix defaults wrong: %+v", cfg.Safety)
	}
	if cfg.Safety.WarnBroadV4 != 20 {
		t.Fatalf("warn broad defaults wrong: %+v", cfg.Safety)
	}
	if cfg.MikroTik.Port != 443 {
		t.Fatalf("ssl port default wrong: %d", cfg.MikroTik.Port)
	}
	if cfg.Firewall.AddressList != "TO-VPN" || cfg.Firewall.CommentPrefix != "AUTO" {
		t.Fatalf("firewall defaults wrong: %+v", cfg.Firewall)
	}
	if cfg.Scheduler.MaxConcurrent != 3 || cfg.Retry.MaxAttempts != 3 {
		t.Fatalf("defaults wrong: %+v %+v", cfg.Scheduler, cfg.Retry)
	}
}

func TestValidateRequiresAddressList(t *testing.T) {
	body := `
timezone: UTC
mikrotik: {host: h, username: u, password: p}
schedules: {global: "every 6h"}
`
	if _, err := Load(writeTempConfig(t, body)); err == nil {
		t.Fatal("expected error for missing firewall.address_list")
	}

	bad := `
timezone: UTC
mikrotik: {host: h, username: u, password: p}
firewall: {address_list: "bad list!"}
schedules: {global: "every 6h"}
`
	if _, err := Load(writeTempConfig(t, bad)); err == nil {
		t.Fatal("expected error for invalid address_list name")
	}
}

func TestENVOverride(t *testing.T) {
	path := writeTempConfig(t, minimalYAML)
	t.Setenv("MRS_MIKROTIK_PASSWORD", "from-env")
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.MikroTik.Password != "from-env" {
		t.Fatalf("ENV must override config, got %q", cfg.MikroTik.Password)
	}
}

func TestValidateRejectsBadServiceName(t *testing.T) {
	path := writeTempConfig(t, `
timezone: UTC
mikrotik: {host: h, username: u, password: p}
schedules: {global: "every 6h"}
services: ["Bad Name"]
`)
	if _, err := Load(path); err == nil {
		t.Fatal("expected invalid service name rejection")
	}
}

func TestSetAndGetPathAndSave(t *testing.T) {
	path := writeTempConfig(t, minimalYAML)
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := cfg.Set("schedules.services.cloudflare.schedule", "every 1h"); err != nil {
		t.Fatalf("set: %v", err)
	}
	if err := cfg.Save(); err != nil {
		t.Fatalf("save: %v", err)
	}
	reloaded, err := Load(path)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if got := reloaded.EffectiveSchedule("cloudflare"); got != "every 1h" {
		t.Fatalf("persisted schedule wrong: %q", got)
	}
	v, err := reloaded.GetPath("schedules.services.cloudflare.schedule")
	if err != nil || v != "every 1h" {
		t.Fatalf("GetPath: %v %v", v, err)
	}
}

func TestValidateSSLPortMismatch(t *testing.T) {
	cfg := &Config{
		MikroTik:  MikroTikConfig{Host: "h", Username: "u", Port: 443, UseSSL: false},
		Firewall:  FirewallConfig{AddressList: "TO-VPN"},
		Schedules: SchedulesConfig{Global: "every 6h"},
	}
	setDefaults(cfg)
	if err := cfg.Validate(); err == nil {
		t.Fatal("expected rejection of use_ssl=false + port=443")
	}
	cfg.MikroTik.UseSSL = true
	if err := cfg.Validate(); err != nil {
		t.Fatalf("use_ssl=true + port=443 must pass: %v", err)
	}
	cfg.MikroTik = MikroTikConfig{Host: "h", Username: "u", Port: 80}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("use_ssl=false + port=80 must pass: %v", err)
	}
}

func TestValidateServiceNameDomains(t *testing.T) {
	ok := []string{"instagram", "youtube.com", "rutor.info", "8.8.8.8", "cdn.fastly.net", "a-b_c"}
	bad := []string{".youtube.com", "youtube.", "you..tube", "UPPER.com", "bad name"}
	for _, n := range ok {
		if !ValidateServiceName(n) {
			t.Errorf("ValidateServiceName(%q) = false, want true", n)
		}
	}
	for _, n := range bad {
		if ValidateServiceName(n) {
			t.Errorf("ValidateServiceName(%q) = true, want false", n)
		}
	}
}

func TestSetGetSnakeCaseKeys(t *testing.T) {
	path := writeTempConfig(t, minimalYAML)
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}

	cases := []struct{ key, value string }{
		{"mikrotik.use_ssl", "false"},
		{"mikrotik.verify_ssl", "false"},
		{"firewall.address_list", "TO-VPN2"},
		{"telegram.bot_token", "123:abc"},
		{"telegram.chat_id", "42"},
		{"safety.max_delete_ratio", "0.7"},
		{"safety.min_prefix_v4", "16"},
		{"scheduler.max_concurrent", "5"},
		{"scheduler.cache_ttl", "2h"},
		{"external.http_timeout", "20s"},
	}
	for _, c := range cases {
		if err := cfg.Set(c.key, c.value); err != nil {
			t.Fatalf("Set(%q): %v", c.key, err)
		}
	}

	if cfg.MikroTik.UseSSL {
		t.Fatal("mikrotik.use_ssl not applied")
	}
	if cfg.Firewall.AddressList == "" {
		t.Fatal("firewall.address_list lost")
	}
	if cfg.Telegram.BotToken != "123:abc" {
		t.Fatal("bot_token not applied")
	}
	if got, _ := cfg.GetPath("safety.min_prefix_v4"); fmt.Sprint(got) != "16" {
		t.Fatalf("GetPath min_prefix_v4 = %v", got)
	}
	if got, _ := cfg.GetPath("scheduler.cache_ttl"); fmt.Sprint(got) != "2h0m0s" {
		t.Fatalf("GetPath cache_ttl = %v", got)
	}
}

func TestFilePermissionsRejected(t *testing.T) {
	path := writeTempConfig(t, minimalYAML)
	_ = os.Chmod(path, 0o644)
	t.Setenv("MRS_REQUIRE_FILE_PERMS", "1")
	if _, err := Load(path); err == nil {
		t.Fatal("expected 0600 permission enforcement")
	}
}

func TestSaveFallsBackWhenRenameUnavailable(t *testing.T) {
	// Эмуляция bind-mount в Docker: rename -> "device or resource busy".
	path := writeTempConfig(t, minimalYAML)
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}

	orig := renameFunc
	renameFunc = func(oldname, newname string) error {
		return fmt.Errorf("rename %s %s: device or resource busy", oldname, newname)
	}
	t.Cleanup(func() { renameFunc = orig })

	if err := cfg.Set("mikrotik.rate_limit", "5"); err != nil {
		t.Fatal(err)
	}
	if err := cfg.Save(); err != nil {
		t.Fatalf("save with rename fallback: %v", err)
	}

	reloaded, err := Load(path)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if got := reloaded.MikroTik.RateLimit; got != 5 {
		t.Fatalf("persisted rate_limit = %d, want 5", got)
	}
}

func TestSaveFallsBackWhenTempCreationDenied(t *testing.T) {
	// Эмуляция bind-mount в Docker Desktop: в каталоге нельзя создавать
	// новые файлы ("open config.yaml.tmp: permission denied"), а сам
	// файл конфигурации перезаписываемый.
	path := writeTempConfig(t, minimalYAML)
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}

	// Каталог на месте temp-файла: os.WriteFile(path.tmp) всегда падает.
	tmpBlock := path + ".tmp"
	if err := os.Mkdir(tmpBlock, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(tmpBlock) })

	if err := cfg.Set("mikrotik.rate_limit", "7"); err != nil {
		t.Fatal(err)
	}
	if err := cfg.Save(); err != nil {
		t.Fatalf("save with in-place fallback: %v", err)
	}

	reloaded, err := Load(path)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if got := reloaded.MikroTik.RateLimit; got != 7 {
		t.Fatalf("persisted rate_limit = %d, want 7", got)
	}
}

func TestEffectiveSchedulePriority(t *testing.T) {
	cfg := &Config{Schedules: SchedulesConfig{
		Global: "every 6h",
		Services: map[string]ServiceSchedule{
			"instagram": {Schedule: "every 1h"},
		},
	}}
	if got := cfg.EffectiveSchedule("instagram"); got != "every 1h" {
		t.Fatalf("service override must win: %s", got)
	}
	delete(cfg.Schedules.Services, "instagram")
	if got := cfg.EffectiveSchedule("instagram"); got != "every 6h" {
		t.Fatalf("global fallback: %s", got)
	}
	if got := cfg.EffectiveSchedule("other"); got != "every 6h" {
		t.Fatalf("global fallback: %s", got)
	}
}

func TestIsValidMethod(t *testing.T) {
	for _, m := range []string{"cdn", "asn", "dynamic", "whois", "static_url", "cdn ", " asn", "dynamic,whois"} {
		if !IsValidMethod(m) {
			t.Errorf("IsValidMethod(%q)=false, want true", m)
		}
	}
	for _, m := range []string{"", "bogus", "CDN", "dynamic,,whois", "dynamic,bogus"} {
		if IsValidMethod(m) {
			t.Errorf("IsValidMethod(%q)=true, want false", m)
		}
	}
}

func TestScheduleForFallback(t *testing.T) {
	c := &Config{}
	c.Schedules.Global = "every 6h"
	c.Schedules.Services = map[string]ServiceSchedule{
		"instagram": {Schedule: "daily at 03:00"},
	}

	if got := c.ScheduleFor("instagram"); got != "daily at 03:00" {
		t.Fatalf("ScheduleFor service override=%q", got)
	}
	if got := c.ScheduleFor("other"); got != "every 6h" {
		t.Fatalf("ScheduleFor fallback=%q", got)
	}
}

func TestReplaceGlobalSchedule(t *testing.T) {
	path := writeTempConfig(t, `
timezone: UTC
mikrotik: {host: h, username: u, password: p}
firewall: {address_list: "TO-VPN"}
schedules:
  global: "every 6h"
services: [instagram]
`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}

	if err := cfg.ReplaceGlobalSchedule("every 12h"); err != nil {
		t.Fatalf("ReplaceGlobalSchedule: %v", err)
	}

	reloaded, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if reloaded.Schedules.Global != "every 12h" {
		t.Fatalf("global not persisted: %q", reloaded.Schedules.Global)
	}
	if got := reloaded.EffectiveSchedule("instagram"); got != "every 12h" {
		t.Fatalf("effective after global update: %q", got)
	}
}

func TestVerifySNIEnabledDefault(t *testing.T) {
	var c Config
	if !c.Safety.VerifySNIEnabled() {
		t.Fatal("default must be enabled")
	}
	off := false
	c.Safety.VerifySNI = &off
	if c.Safety.VerifySNIEnabled() {
		t.Fatal("explicit false must disable")
	}
}

func validBase(t *testing.T) *Config {
	t.Helper()
	c := &Config{}
	c.MikroTik.Host = "h"
	c.MikroTik.Username = "u"
	c.Firewall.AddressList = "TO-VPN"
	c.Schedules.Global = "manual"
	setDefaults(c)
	return c
}

func TestValidateSafetyRanges(t *testing.T) {
	if err := validBase(t).Validate(); err != nil {
		t.Fatalf("defaults must validate: %v", err)
	}
	cases := []struct {
		name string
		set  func(c *Config)
	}{
		{"min_prefix_v4 low", func(c *Config) { c.Safety.MinPrefixV4 = 7 }},
		{"min_prefix_v4 high", func(c *Config) { c.Safety.MinPrefixV4 = 33 }},
		{"warn_broad_v4 low", func(c *Config) { c.Safety.WarnBroadV4 = 7 }},
		{"warn_broad_v4 high", func(c *Config) { c.Safety.WarnBroadV4 = 33 }},
		{"max_asn_prefixes zero", func(c *Config) { c.Safety.MaxASNPrefixes = 0 }},
		{"max_asn_prefixes huge", func(c *Config) { c.Safety.MaxASNPrefixes = 10001 }},
		{"max_delete_ratio negative", func(c *Config) { c.Safety.MaxDeleteRatio = -0.1 }},
		{"max_delete_ratio above one", func(c *Config) { c.Safety.MaxDeleteRatio = 1.1 }},
		{"require_confirmation_over negative", func(c *Config) { c.Safety.RequireConfirmationOver = -1 }},
	}
	for _, tc := range cases {
		c := validBase(t)
		tc.set(c)
		if err := c.Validate(); err == nil {
			t.Errorf("%s: expected validation error", tc.name)
		}
	}
	// Границы допустимы.
	c := validBase(t)
	c.Safety.MinPrefixV4 = 32
	c.Safety.WarnBroadV4 = 8
	c.Safety.MaxASNPrefixes = 10000
	c.Safety.MaxDeleteRatio = 1
	if err := c.Validate(); err != nil {
		t.Fatalf("boundaries must validate: %v", err)
	}
}

func TestValidateCompositeMethods(t *testing.T) {
	for _, m := range []string{"dynamic,whois", "dynamic+cdn", "cdn;whois", "dynamic, whois"} {
		c := validBase(t)
		c.Overrides = map[string]ServiceOverride{"svc": {Method: m}}
		if err := c.Validate(); err != nil {
			t.Errorf("composite method %q must validate: %v", m, err)
		}
		if !IsValidMethod(m) {
			t.Errorf("IsValidMethod(%q)=false, want true", m)
		}
	}
	for _, m := range []string{"dynamic,,whois", "dynamic,bogus", "dynamic,"} {
		c := validBase(t)
		c.Overrides = map[string]ServiceOverride{"svc": {Method: m}}
		if err := c.Validate(); err == nil {
			t.Errorf("method %q must be rejected", m)
		}
		if IsValidMethod(m) {
			t.Errorf("IsValidMethod(%q)=true, want false", m)
		}
	}
}

func TestSplitMethods(t *testing.T) {
	got := SplitMethods("dynamic,whois+cdn; asn")
	want := []string{"dynamic", "whois", "cdn", "asn"}
	if len(got) != len(want) {
		t.Fatalf("SplitMethods = %v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("SplitMethods = %v, want %v", got, want)
		}
	}
}

func TestAllowHostRoutesDefaultsTrue(t *testing.T) {
	var s SafetyConfig
	if !s.AllowHostRoutesEnabled() {
		t.Fatal("nil AllowHostRoutes must default to enabled")
	}
	path := writeTempConfig(t, minimalYAML)
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.Safety.AllowHostRoutesEnabled() {
		t.Fatal("loaded default must enable host routes")
	}
}

func TestSetAllowHostRoutesPointer(t *testing.T) {
	path := writeTempConfig(t, minimalYAML)
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := cfg.Set("safety.allow_host_routes", "false"); err != nil {
		t.Fatalf("Set: %v", err)
	}
	if cfg.Safety.AllowHostRoutesEnabled() {
		t.Fatal("expected host routes disabled")
	}
	if got, _ := cfg.GetPath("safety.allow_host_routes"); fmt.Sprint(got) != "false" {
		t.Fatalf("GetPath allow_host_routes = %v", got)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("explicit false must validate: %v", err)
	}
	if err := cfg.Set("safety.allow_host_routes", "true"); err != nil {
		t.Fatalf("Set: %v", err)
	}
	if !cfg.Safety.AllowHostRoutesEnabled() {
		t.Fatal("expected host routes enabled")
	}
}

// TestReplaceFromLockedCopiesAll — все 17 полей, mu/path не трогаем.
func TestReplaceFromLockedCopiesAll(t *testing.T) {
	dst := &Config{}
	srcYAML := `
timezone: Europe/Moscow
cache_path: /tmp/x.db
logging: {level: debug}
mikrotik: {host: 10.0.0.1, port: 8728, username: u, password: p}
firewall: {address_list: L, comment_prefix: C}
telegram: {enabled: true, bot_token: t}
web: {enabled: true, listen: 127.0.0.1:9999}
scheduler: {max_concurrent: 5}
safety: {max_delete_ratio: 0.1}
retry: {max_attempts: 2}
external: {resolver: 9.9.9.9:53}
snapshots: {enabled: true}
schedules: {global: manual}
suggest: {v2fly_enabled: true, urlscan_mode: key}
presets: {update_schedule: daily}
services: [a, b]
overrides: {a: {method: whois}}
`
	var src Config
	if err := yaml.Unmarshal([]byte(srcYAML), &src); err != nil {
		t.Fatal(err)
	}
	dst.ReplaceFromLocked(&src)
	if dst.Timezone != "Europe/Moscow" || dst.CachePath != "/tmp/x.db" {
		t.Fatal("base fields not copied")
	}
	if len(dst.Services) != 2 || dst.Overrides["a"].Method != "whois" {
		t.Fatal("services/overrides not copied")
	}
	if !dst.Suggest.V2flyEnabled || dst.Suggest.UrlscanMode != "key" {
		t.Fatal("suggest not copied")
	}
	if dst.Presets.UpdateSchedule != "daily" {
		t.Fatal("presets not copied")
	}
	if dst.Scheduler.MaxConcurrent != 5 || dst.Logging.Level != "debug" {
		t.Fatal("nested structs not copied")
	}
}

// TestReplaceFromConcurrent — подмена параллельно с чтениями (SIGHUP-путь).
func TestReplaceFromConcurrent(t *testing.T) {
	dst := &Config{Services: []string{"a"}}
	var wg sync.WaitGroup
	stop := make(chan struct{})
	for g := 0; g < 4; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 100; i++ {
				select {
				case <-stop:
					return
				default:
				}
				dst.RLock()
				_ = len(dst.Services)
				_ = dst.EffectiveSchedule("a")
				dst.RUnlock()
			}
		}()
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 20; i++ {
			src := &Config{Services: []string{"b"}}
			dst.Lock()
			dst.ReplaceFromLocked(src)
			dst.Unlock()
		}
		close(stop)
	}()
	wg.Wait()
}
