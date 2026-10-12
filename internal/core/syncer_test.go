package core

import (
	"context"
	"encoding/json"
	"fmt"
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
	t.Cleanup(func() { _ = cache.Close() })

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

func TestRemoveServiceClearsDegradedAndSchedule(t *testing.T) {
	m := &mockRouter{t: t}
	s, cfg := newTestSyncer(t, m.serve(nil, false))

	// Персональное расписание + флаг деградации для сервиса.
	if cfg.Schedules.Services == nil {
		cfg.Schedules.Services = map[string]config.ServiceSchedule{}
	}
	cfg.Schedules.Services["yt"] = config.ServiceSchedule{Schedule: "daily at 03:00"}
	s.MarkDegraded("yt")

	if err := s.RemoveService(context.Background(), "yt", false); err != nil {
		t.Fatalf("remove: %v", err)
	}
	if s.IsDegraded("yt") {
		t.Fatal("degraded flag survives service removal")
	}
	if _, ok := cfg.Schedules.Services["yt"]; ok {
		t.Fatal("per-service schedule survives service removal")
	}
	for _, sv := range cfg.Services {
		if sv == "yt" {
			t.Fatal("service still in config")
		}
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

// TestGetServiceOverrideConcurrent — чтение override параллельно с записью:
// бывший fatal crash concurrent map read/write (H1 аудита).
func TestGetServiceOverrideConcurrent(t *testing.T) {
	m := &mockRouter{t: t}
	s, _ := newTestSyncer(t, m.serve(nil, false))

	var wg sync.WaitGroup
	stop := make(chan struct{})
	for g := 0; g < 4; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 50; i++ {
				select {
				case <-stop:
					return
				default:
				}
				_ = s.GetServiceOverride("yt")
				_ = s.ListServices()
				_ = s.ServiceSchedules()
			}
		}()
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 30; i++ {
			_, _ = s.UpdateServiceOverride(context.Background(), "yt", strPtr("dynamic"), []string{"a.example"})
			_, _ = s.UpdateServiceOverride(context.Background(), "yt", nil, nil)
		}
		close(stop)
	}()
	wg.Wait()
}

// TestUpdateServiceOverrideDomainCap — больше 5000 доменов отклоняется в ядре,
// а не только в веб-хендлере (обход через прямые вызовы).
func TestUpdateServiceOverrideDomainCap(t *testing.T) {
	m := &mockRouter{t: t}
	s, _ := newTestSyncer(t, m.serve(nil, false))

	big := make([]string, 0, 5001)
	for i := 0; i < 5001; i++ {
		big = append(big, "h"+strconv.Itoa(i)+".example")
	}
	if _, err := s.UpdateServiceOverride(context.Background(), "yt", nil, big); err == nil {
		t.Fatal("5001 domains must be rejected")
	}
	if _, err := s.UpdateServiceOverride(context.Background(), "yt", nil, []string{"ok.example"}); err != nil {
		t.Fatalf("small update must pass: %v", err)
	}
}

// TestApplyPresetRefreshKeepsManual — ручные домены переживают refresh,
// diff честный, повторный refresh — пустой diff.
func TestApplyPresetRefreshKeepsManual(t *testing.T) {
	m := &mockRouter{t: t}
	s, _ := newTestSyncer(t, m.serve(nil, false))

	// Без override вообще (nil-map): refresh обязан работать, а не паниковать.
	if _, _, err := s.ApplyPresetRefresh(context.Background(), "yt", "youtube", false); err != nil {
		t.Fatalf("refresh without override: %v", err)
	}
	ov := s.GetServiceOverride("yt")
	if ov.Preset != "youtube" || len(ov.Generated) == 0 {
		t.Fatalf("preset not recorded: %+v", ov)
	}
	before := append([]string(nil), ov.Domains...)

	// Ручной домен поверх.
	manual := append(append([]string(nil), before...), "my.manual.example")
	if _, err := s.UpdateServiceOverride(context.Background(), "yt", nil, manual); err != nil {
		t.Fatalf("manual add: %v", err)
	}
	added, removed, err := s.ApplyPresetRefresh(context.Background(), "yt", "", false)
	if err != nil {
		t.Fatalf("re-refresh: %v", err)
	}
	if len(added) != 0 || len(removed) != 0 {
		t.Fatalf("re-refresh must be empty diff, got +%v -%v", added, removed)
	}
	ov2 := s.GetServiceOverride("yt")
	found := false
	for _, d := range ov2.Domains {
		if d == "my.manual.example" {
			found = true
		}
	}
	if !found {
		t.Fatal("manual domain lost after refresh")
	}
}

// reportVerificationStep отображает долю done/total доменов в отрезок
// [lo,hi] и несёт подпись текущего домена. Верхняя граница не превышается.
func TestReportVerificationStep(t *testing.T) {
	s, _ := newTestSyncer(t, noopRouter)

	cases := []struct {
		done, total int
		wantPct     int
	}{
		{0, 4, 50}, // начало отрезка [50,52]
		{1, 4, 50},
		{2, 4, 51},
		{3, 4, 51},
		{4, 4, 52}, // конец отрезка
		{5, 4, 52}, // переполнение зажато верхней границей
	}
	for _, c := range cases {
		s.reportVerificationStep("yt", "контроль покрытия", false, 50, 52, c.done, c.total, "DNS: x")
		got := s.ProgressSnapshot()["yt"]
		if got.Percent != c.wantPct {
			t.Fatalf("done=%d total=%d: pct=%d want %d", c.done, c.total, got.Percent, c.wantPct)
		}
		if got.Detail != fmt.Sprintf("DNS: x (%d/%d)", c.done, c.total) {
			t.Fatalf("detail=%q", got.Detail)
		}
	}

	// Нет доменов — шаг не двигает прогресс.
	s.clearProgress("yt")
	s.reportVerificationStep("yt", "контроль покрытия", false, 50, 52, 0, 0, "DNS: x")
	if _, ok := s.ProgressSnapshot()["yt"]; ok {
		t.Fatal("zero total must not set progress")
	}
}
