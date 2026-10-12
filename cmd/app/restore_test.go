package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// minimalTestConfig — валидный конфиг для CLI-тестов (свой cache_path,
// чтобы не создавать файлы в рабочем каталоге).
func minimalTestConfig(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	body := "timezone: UTC\n" +
		"mikrotik: {host: 127.0.0.1, port: 9, username: api, password: x}\n" +
		"firewall: {address_list: TO-VPN, comment_prefix: AUTO}\n" +
		"schedules: {global: manual}\n" +
		"services: []\n" +
		"cache_path: " + filepath.Join(dir, "c.db") + "\n"
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestRestoreCmdValidation — restore без флагов и с битым путём:
// чистые ошибки вместо паники index out of range (args[1]).
func TestRestoreCmdValidation(t *testing.T) {
	oldCfg := cfgPath
	t.Cleanup(func() { cfgPath = oldCfg })
	cfgPath = minimalTestConfig(t)

	cmd := restoreCmd()
	cmd.SetArgs([]string{"svc", "--force"})
	if err := cmd.Execute(); err == nil || !strings.Contains(err.Error(), "--from-file") {
		t.Fatalf("want --from-file hint, got %v", err)
	}

	cmd2 := restoreCmd()
	cmd2.SetArgs([]string{"svc", "--force", "--from-file", filepath.Join(t.TempDir(), "nope.json")})
	if err := cmd2.Execute(); err == nil || !strings.Contains(err.Error(), "read backup file") {
		t.Fatalf("want read error, got %v", err)
	}
}
