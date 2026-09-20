package config

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
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
	if cfg.MikroTik.Port != 443 {
		t.Fatalf("ssl port default wrong: %d", cfg.MikroTik.Port)
	}
	if cfg.Scheduler.MaxConcurrent != 3 || cfg.Retry.MaxAttempts != 3 {
		t.Fatalf("defaults wrong: %+v %+v", cfg.Scheduler, cfg.Retry)
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

func TestSetGetSnakeCaseKeys(t *testing.T) {
	path := writeTempConfig(t, minimalYAML)
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}

	cases := []struct{ key, value string }{
		{"mikrotik.use_ssl", "false"},
		{"mikrotik.verify_ssl", "false"},
		{"mikrotik.routing_table", "main2"},
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
	if cfg.MikroTik.RoutingTable != "main2" {
		t.Fatalf("routing_table: %q", cfg.MikroTik.RoutingTable)
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
	os.Chmod(path, 0o644)
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

	if err := cfg.Set("mikrotik.distance", 5); err != nil {
		t.Fatal(err)
	}
	if err := cfg.Save(); err != nil {
		t.Fatalf("save with rename fallback: %v", err)
	}

	reloaded, err := Load(path)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if got := reloaded.MikroTik.Distance; got != 5 {
		t.Fatalf("persisted distance = %d, want 5", got)
	}
}

func TestEffectiveSchedulePriority(t *testing.T) {
	cfg := &Config{Schedules: SchedulesConfig{
		Global: "every 6h",
		Groups: map[string]GroupConfig{"social": {Schedule: "daily at 03:00", Services: []string{"instagram"}}},
		Services: map[string]ServiceSchedule{
			"instagram": {Schedule: "every 1h"},
		},
	}}
	if got := cfg.EffectiveSchedule("instagram"); got != "every 1h" {
		t.Fatalf("service override must win: %s", got)
	}
	cfg.Services = []string{"telegram"}
	delete(cfg.Schedules.Services, "instagram")
	if got := cfg.EffectiveSchedule("instagram"); got != "daily at 03:00" {
		t.Fatalf("group must apply: %s", got)
	}
	if got := cfg.EffectiveSchedule("other"); got != "every 6h" {
		t.Fatalf("global fallback: %s", got)
	}
}
