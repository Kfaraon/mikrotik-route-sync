package bot

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"golang.org/x/time/rate"

	"github.com/Kfaraon/mikrotik-route-sync/internal/config"
	"github.com/Kfaraon/mikrotik-route-sync/internal/core"
	"github.com/Kfaraon/mikrotik-route-sync/internal/notifier"
	"github.com/Kfaraon/mikrotik-route-sync/internal/storage"
)

func testSyncer(t *testing.T) *core.Syncer {
	t.Helper()
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	body := "timezone: UTC\nmikrotik: {host: 127.0.0.1, port: 9, username: api, password: p}\n" +
		"firewall: {address_list: TO-VPN, comment_prefix: AUTO}\n" +
		"schedules: {global: 'every 6h'}\nretry: {max_attempts: 1, base_delay: 1ms, max_delay: 1ms}\nservices: [svc]\n"
	if err := os.WriteFile(cfgPath, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	cache, err := storage.Open(filepath.Join(dir, "c.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cache.Close() })
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	s, err := core.NewSyncer(cfg, log, cache, &notifier.NoopNotifier{})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// TestSchedulesTextConcurrent — schedulesText параллельно с мутациями:
// бывший дедлок вложенного RLock (бот держал чтение, ListServices брал ещё).
func TestSchedulesTextConcurrent(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	syncer := testSyncer(t)
	b := &Bot{syncer: syncer, log: log, limits: map[int64]*rate.Limiter{}}
	// Сид override: refresh без него падает паникой nil-map (заведённый баг).
	if _, err := syncer.UpdateServiceOverride(context.Background(), "svc", nil, []string{"seed.example"}); err != nil {
		t.Fatal(err)
	}

	var wg sync.WaitGroup
	stop := make(chan struct{})
	for g := 0; g < 4; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 20; i++ {
				select {
				case <-stop:
					return
				default:
				}
				_ = b.schedulesText()
				if i%10 == 0 {
					_ = b.statusText(context.Background())
				}
			}
		}()
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 15; i++ {
			_, _ = b.syncer.UpdateServiceOverride(context.Background(), "svc", nil, []string{"x.example"})
			_, _, _ = b.syncer.ApplyPresetRefresh(context.Background(), "svc", "youtube", false)
		}
		close(stop)
	}()
	wg.Wait()
}
