//go:build !windows

package main

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// TestWritePidFileStale — протухший pid (процесса нет) перезаписывается.
func TestWritePidFileStale(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml.pid")
	// Заведомо мёртвый pid.
	if err := os.WriteFile(path, []byte("2147483646"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := writePidFile(path); err != nil {
		t.Fatalf("writePidFile over stale: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(string(data)) != strconv.Itoa(os.Getpid()) {
		t.Fatalf("pid file = %q, want %d", strings.TrimSpace(string(data)), os.Getpid())
	}
}

// TestWritePidFileAliveOther — живой чужой pid: отказ, файл не затёрт.
func TestWritePidFileAliveOther(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml.pid")
	// pid=1 существует всегда (init), но наш тест-процесс не 1.
	if err := os.WriteFile(path, []byte("1"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := writePidFile(path); err == nil {
		t.Fatal("ожидалась ошибка при живом чужом pid")
	}
	data, _ := os.ReadFile(path)
	if strings.TrimSpace(string(data)) != "1" {
		t.Fatalf("чужой pid затёрт: %q", string(data))
	}
}

// TestProcessAlive — sanity проверки живости.
func TestProcessAlive(t *testing.T) {
	if !processAlive(os.Getpid()) {
		t.Fatal("текущий процесс должен быть жив")
	}
	if processAlive(2147483646) {
		t.Fatal("мёртвый pid не должен считаться живым")
	}
	if processAlive(0) {
		t.Fatal("pid 0 не должен считаться живым")
	}
}
