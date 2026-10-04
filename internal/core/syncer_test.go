package core

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/Kfaraon/mikrotik-route-sync/internal/addresslist"
	"github.com/Kfaraon/mikrotik-route-sync/internal/config"
	"github.com/Kfaraon/mikrotik-route-sync/internal/notifier"
	"github.com/Kfaraon/mikrotik-route-sync/internal/storage"
)

type mockRouter struct {
	t       *testing.T
	mu      sync.Mutex
	deleted []string
}

func (m *mockRouter) serve(list []addresslist.Entry, failDelete bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/remove"):
			var body struct {
				Numbers string `json:"numbers"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			if failDelete {
				http.Error(w, `{"detail":"boom","error":400,"message":"Bad Request"}`, http.StatusBadRequest)
				return
			}
			m.mu.Lock()
			m.deleted = append(m.deleted, body.Numbers)
			m.mu.Unlock()
			_, _ = w.Write([]byte(`[]`))
		case r.Method == http.MethodDelete:
			// legacy fallback — при failDelete он тоже должен отказать
			if failDelete {
				http.Error(w, `{"detail":"boom","error":400,"message":"Bad Request"}`, http.StatusBadRequest)
				return
			}
			id := r.URL.Query().Get(".id")
			m.mu.Lock()
			m.deleted = append(m.deleted, id)
			m.mu.Unlock()
			_, _ = w.Write([]byte(`[]`))
		case r.Method == http.MethodPut:
			_, _ = w.Write([]byte(`{"id":"*new"}`))
		default:
			_ = json.NewEncoder(w).Encode(list)
		}
	}
}

func newTestSyncer(t *testing.T, handler http.HandlerFunc) (*Syncer, *config.Config) {
	t.Helper()

	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)

	host, portStr, err := net.SplitHostPort(strings.TrimPrefix(srv.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	port, _ := strconv.Atoi(portStr)

	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	body := "timezone: UTC\nmikrotik: {host: " + host + ", port: " + portStr + ", username: api, password: p}\n" +
		"firewall: {address_list: TO-VPN, comment_prefix: AUTO}\n" +
		"schedules: {global: 'every 6h'}\nservices: [yt]\n"
	if err := os.WriteFile(cfgPath, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	cfg.MikroTik.Port = port

	cache, err := storage.Open(filepath.Join(dir, "c.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cache.Close() })

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	s, err := NewSyncer(cfg, log, cache, &notifier.NoopNotifier{})
	if err != nil {
		t.Fatal(err)
	}
	return s, cfg
}

func TestRemoveServicePurgeDeletesEntries(t *testing.T) {
	entries := []addresslist.Entry{
		{ID: "*1", Address: "8.8.8.0/24", List: "TO-VPN", Comment: "AUTO:yt"},
		{ID: "*2", Address: "1.1.1.0/24", List: "TO-VPN", Comment: "AUTO:yt"},
		{ID: "*3", Address: "9.9.9.0/24", List: "TO-VPN", Comment: "AUTO:other"},
		{ID: "*4", Address: "5.5.5.0/24", List: "TO-VPN"},
	}
	m := &mockRouter{t: t}
	s, cfg := newTestSyncer(t, m.serve(entries, false))

	if err := s.RemoveService(context.Background(), "yt", true); err != nil {
		t.Fatalf("remove: %v", err)
	}
	// Только AUTO:yt записи: *1,*2 (не *3 другого сервиса и не *4 без комментария)
	if len(m.deleted) != 2 || m.deleted[0] != "*1" || m.deleted[1] != "*2" {
		t.Fatalf("wrong entries deleted: %v", m.deleted)
	}
	for _, sv := range cfg.Services {
		if sv == "yt" {
			t.Fatal("service still in config")
		}
	}
}

func TestRemoveServicePurgeFailsLoud(t *testing.T) {
	entries := []addresslist.Entry{
		{ID: "*1", Address: "8.8.8.0/24", List: "TO-VPN", Comment: "AUTO:yt"},
	}
	m := &mockRouter{t: t}
	s, cfg := newTestSyncer(t, m.serve(entries, true))

	err := s.RemoveService(context.Background(), "yt", true)
	if err == nil {
		t.Fatal("expected error when router rejects delete")
	}
	found := false
	for _, sv := range cfg.Services {
		if sv == "yt" {
			found = true
		}
	}
	if !found {
		t.Fatal("service must stay in config when purge failed")
	}
}

func TestListGlobalEntriesParsesEntries(t *testing.T) {
	m := &mockRouter{t: t}
	s, _ := newTestSyncer(t, m.serve([]addresslist.Entry{
		{ID: "*1", Address: "8.8.8.0/24", List: "TO-VPN", Comment: "AUTO:yt"},
		{ID: "*4", Address: "5.5.5.0/24", List: "TO-VPN"},
	}, false))
	got, err := s.ListGlobalEntries(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("expected 2, got %+v", got)
	}
}
