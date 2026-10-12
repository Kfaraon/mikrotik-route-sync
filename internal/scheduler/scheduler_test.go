package scheduler

import (
	"context"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/Kfaraon/mikrotik-route-sync/internal/config"
)

func TestToCron(t *testing.T) {
	cases := map[string]string{
		"0 */6 * * *":              "0 */6 * * *",
		"daily at 03:00":           "0 3 * * *",
		"weekly on sunday at 4:05": "5 4 * * 0",
	}
	for in, want := range cases {
		got, ok := toCron(in)
		if !ok || got != want {
			t.Errorf("toCron(%q) = %q,%v want %q", in, got, ok, want)
		}
	}
}

func TestInterval(t *testing.T) {
	d, ok := interval("every 6h")
	if !ok || d != 6*time.Hour {
		t.Fatalf("interval every 6h = %v,%v", d, ok)
	}
	if _, ok := interval("manual"); ok {
		t.Fatal("manual is not an interval")
	}
}

func TestValidSpec(t *testing.T) {
	for _, spec := range []string{"manual", "disabled", "inherit", "every 30m", "daily at 03:00", "0 3 * * *"} {
		if !ValidSpec(spec) {
			t.Errorf("ValidSpec(%q) = false", spec)
		}
	}
	for _, spec := range []string{"bogus", "every", "daily at 99:99", "bad spec here!!"} {
		if ValidSpec(spec) {
			t.Errorf("ValidSpec(%q) = true", spec)
		}
	}
}

func TestPurgeInterval(t *testing.T) {
	if d, ok := purgeInterval("every 1h"); !ok || d != time.Hour {
		t.Fatalf("purgeInterval = %v,%v", d, ok)
	}
	if _, ok := purgeInterval(""); ok {
		t.Fatal("empty spec must be not ok")
	}
}

func TestRunRecoversPanic(t *testing.T) {
	// nil syncer вызовет панику при вызове SyncOneResult — recover должен её поймать.
	s := &Scheduler{
		log:    slog.Default(),
		syncer: nil,
	}
	// Не должно паниковать — recover внутри run ловит панику.
	s.run(context.Background(), "test-service", "every 1h")
}

// E4: wg.Add в dispatch не должен гоняться с wg.Wait в Stop. Гвардия
// runMu/runActive запрещает Add после выключения: -race ловит нарушение.
// O3: Push должен разбудить ожидающий runner, signal — неблокирующий.
func TestIntervalHeapSignalsOnPush(t *testing.T) {
	h := newIntervalHeap()
	// Пустая куча: сигнала нет.
	select {
	case <-h.notify:
		t.Fatal("unexpected signal on empty heap")
	default:
	}
	h.Push(intervalItem{service: "svc", interval: time.Hour, nextRun: time.Now().Add(time.Hour)})
	select {
	case <-h.notify:
	default:
		t.Fatal("push did not signal")
	}
	// Повторные сигналы не блокируют и не теряют состояние.
	for i := 0; i < 10; i++ {
		h.signal()
	}
	if got := h.Len(); got != 1 {
		t.Fatalf("len=%d, want 1", got)
	}
}

func TestDispatchStopNoWaitGroupRace(t *testing.T) {
	s := &Scheduler{log: slog.Default()} // nil syncer: run паникует и восстанавливается
	s.sem = make(chan struct{}, 4)
	s.semPub.Store(s.sem)
	bctx, bcancel := context.WithCancel(context.Background())
	s.basePub.Store(syncBase{ctx: bctx})
	s.baseCancel = bcancel
	s.runActive = true

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var callers sync.WaitGroup
	for i := 0; i < 200; i++ {
		callers.Add(1)
		go func() {
			defer callers.Done()
			s.dispatch(ctx, "svc", "every 1h")
		}()
	}
	cancel()

	// Эмуляция Stop: выключаем приём и ждём счётчик.
	s.runMu.Lock()
	s.runActive = false
	s.runMu.Unlock()
	s.wg.Wait()
	callers.Wait()
}

// Reload не должен отменять базовый контекст идущего синка, а Stop — должен.
func TestReloadKeepsSyncBaseContext(t *testing.T) {
	s := &Scheduler{log: slog.Default(), cfg: &config.Config{}}
	s.mu.Lock()
	s.started = true
	s.startLocked(true) // создаёт base-поколение
	s.mu.Unlock()
	base1, _ := s.basePub.Load().(syncBase)
	if base1.ctx == nil || base1.invalid {
		t.Fatal("startLocked must publish a valid sync base")
	}

	// Reload пересоздаёт машинерию, но base сохраняется.
	if err := s.ReloadQuiet(); err != nil {
		t.Fatalf("ReloadQuiet: %v", err)
	}
	base2, _ := s.basePub.Load().(syncBase)
	if base2.ctx == nil || base2.invalid {
		t.Fatal("reload must keep a valid sync base")
	}
	if base1.ctx.Err() != nil {
		t.Fatal("reload must not cancel the running sync base context")
	}

	// Stop инвалидирует и отменяет базовый контекст.
	s.Stop(context.Background())
	base3, _ := s.basePub.Load().(syncBase)
	if !base3.invalid {
		t.Fatal("stop must invalidate the sync base")
	}
	if base1.ctx.Err() == nil {
		t.Fatal("stop must cancel the running sync base context")
	}
}
