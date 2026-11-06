package core

// Тесты на безопасность жизненного цикла: удаление сервиса не гоняет с
// синхронизацией, провальные синки попадают в историю, устаревшие managed-
// записи удаляются даже когда набор покрыт чужими записями.

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"

	"github.com/Kfaraon/mikrotik-list-sync/internal/addresslist"
)

// Занятый сервис (идёт синхронизация) — удаление отказывает, не начиная purge.
func TestRemoveServiceRefusesWhenBusy(t *testing.T) {
	m := &mockRouter{t: t}
	s, cfg := newTestSyncer(t, m.serve(nil, false))

	if !s.acquire("yt") {
		t.Fatal("acquire must succeed")
	}

	err := s.RemoveService(context.Background(), "yt", true)
	if err == nil {
		t.Fatal("expected refusal while service is busy")
	}
	if len(m.deleted) != 0 {
		t.Fatalf("purge must not run while busy, deleted=%v", m.deleted)
	}
	found := false
	for _, sv := range cfg.Services {
		if sv == "yt" {
			found = true
		}
	}
	if !found {
		t.Fatal("service must stay in config after refusal")
	}

	s.release("yt")
	if err := s.RemoveService(context.Background(), "yt", true); err != nil {
		t.Fatalf("remove after release: %v", err)
	}
}

// Повторный запуск занятого сервиса — ErrSyncBusy, а не «провал» в истории.
func TestSyncServiceBusyReturnsErrSyncBusy(t *testing.T) {
	fr := newFakeRouter()
	src := &fakeSource{}
	src.set([]string{addrA}, nil)
	s := newE2ESyncer(t, fr, src, true)

	if !s.acquire("demo") {
		t.Fatal("acquire must succeed")
	}
	defer s.release("demo")

	res, err := s.SyncService(context.Background(), "demo", false, false)
	if !errors.Is(err, ErrSyncBusy) {
		t.Fatalf("want ErrSyncBusy, got %v", err)
	}
	if res.Success {
		t.Fatalf("busy skip must not be a success: %+v", res)
	}
	if _, ok := s.History().Last("demo"); ok {
		t.Fatal("busy skip must not create history record")
	}
}

// Провальный синк обязан попасть в историю со статусом failed.
func TestFailedSyncRecordedInHistory(t *testing.T) {
	fr := newFakeRouter()
	src := &fakeSource{}
	src.set(nil, fmt.Errorf("source down"))
	s := newE2ESyncer(t, fr, src, true)

	if _, err := s.SyncService(context.Background(), "demo", false, false); err == nil {
		t.Fatal("expected collect error")
	}

	rec, ok := s.History().Last("demo")
	if !ok {
		t.Fatal("failed sync must be recorded in history")
	}
	if rec.Status != "failed" {
		t.Fatalf("status = %q, want failed", rec.Status)
	}
	if rec.Error == "" {
		t.Fatal("failed record must carry the error text")
	}
}

// Dry-run в историю не пишется: иначе «последняя синхронизация» показывала бы
// тестовый запуск, а health-статус — его фиктивный результат.
func TestDryRunNotRecordedInHistory(t *testing.T) {
	fr := newFakeRouter()
	src := &fakeSource{}
	src.set([]string{addrA}, nil)
	s := newE2ESyncer(t, fr, src, true)

	res, err := s.SyncService(context.Background(), "demo", true, false)
	if err != nil {
		t.Fatalf("dry run: %v", err)
	}
	if !res.DryRun {
		t.Fatalf("expected dry run result: %+v", res)
	}
	if _, ok := s.History().Last("demo"); ok {
		t.Fatal("dry run must not be recorded in history")
	}
}

// Dry-run возвращает план изменений (списки префиксов), чтобы UI показал
// diff сразу после теста, не дожидаясь реальной синхронизации.
func TestDryRunReturnsPlan(t *testing.T) {
	fr := newFakeRouter(
		// Собственная устаревшая запись: она должна попасть в remove.
		addresslist.Entry{ID: "*71", Address: addrB, List: "TO-VPN", Comment: "AUTO:demo", Disabled: "false"},
	)
	src := &fakeSource{}
	src.set([]string{addrA, addrC}, nil)
	s := newE2ESyncer(t, fr, src, true)

	// force: удаляется единственная managed-запись (полное опустошение
	// набора защитой блокируется), а нам нужен именно план с remove.
	res, err := s.SyncService(context.Background(), "demo", true, true)
	if err != nil {
		t.Fatalf("dry run: %v", err)
	}
	if res.Plan == nil {
		t.Fatalf("dry run must return plan: %+v", res)
	}
	if res.Added != len(res.Plan.Add) || res.Removed != len(res.Plan.Remove) ||
		res.Unchanged != len(res.Plan.Unchanged) {
		t.Fatalf("plan lists must match counters: %+v plan=%+v", res, res.Plan)
	}
	if len(res.Plan.Add) != 2 {
		t.Fatalf("want 2 adds, got %v", res.Plan.Add)
	}
	if len(res.Plan.Remove) != 1 || res.Plan.Remove[0] != addrB {
		t.Fatalf("want stale %s in remove, got %v", addrB, res.Plan.Remove)
	}
	// Dry-run не трогает роутер.
	if ops := fr.takeOps(); len(ops) != 0 {
		t.Fatalf("dry run must not mutate router: %v", ops)
	}
}

// План приходит и при реальной синхронизации (для логов/API/истории).
func TestRealSyncReturnsPlan(t *testing.T) {
	fr := newFakeRouter()
	src := &fakeSource{}
	src.set([]string{addrA}, nil)
	s := newE2ESyncer(t, fr, src, true)

	res, err := s.SyncService(context.Background(), "demo", false, false)
	if err != nil {
		t.Fatalf("sync: %v", err)
	}
	if res.Plan == nil || len(res.Plan.Add) != 1 || res.Plan.Add[0] != addrA {
		t.Fatalf("unexpected plan: %+v", res.Plan)
	}
}

// Успешный синк пишет success-запись (регресс: раньше запись была единственной
// и жила внутри тела функции).
func TestSuccessSyncRecordedInHistory(t *testing.T) {
	fr := newFakeRouter()
	src := &fakeSource{}
	src.set([]string{addrA, addrB}, nil)
	s := newE2ESyncer(t, fr, src, true)

	if _, err := s.SyncService(context.Background(), "demo", false, false); err != nil {
		t.Fatalf("sync: %v", err)
	}
	rec, ok := s.History().Last("demo")
	if !ok {
		t.Fatal("successful sync must be recorded")
	}
	if rec.Status != "success" || rec.Error != "" {
		t.Fatalf("unexpected record: %+v", rec)
	}
	if rec.Collected != 2 || rec.Aggregated != 2 {
		t.Fatalf("collected/aggregated not filled: %+v", rec)
	}
	if rec.Method != "static_url" {
		t.Fatalf("method not recorded: %q", rec.Method)
	}
}

// Набор покрыт чужой записью, но у сервиса осталась своя устаревшая запись:
// она должна быть удалена, а не считаться «изменений не требуется».
// Удаление всех своих записей в covered-случае не оголяет трафик: диапазон
// уже закрыт включённой чужой записью, поэтому защита «новый набор пуст»
// здесь неприменима.
func TestStaleManagedRemovedWhenDesiredCoveredByForeign(t *testing.T) {
	fr := newFakeRouter(
		// Чужая (ручная) запись, перекрывающая весь будущий набор.
		addresslist.Entry{ID: "*90", Address: "5.5.5.0/24", List: "TO-VPN", Comment: "Manual", Disabled: "false"},
		// Собственная устаревшая запись сервиса (в новом наборе её нет).
		addresslist.Entry{ID: "*91", Address: addrB, List: "TO-VPN", Comment: "AUTO:demo", Disabled: "false"},
	)
	src := &fakeSource{}
	src.set([]string{"5.5.5.0/24"}, nil)
	s := newE2ESyncer(t, fr, src, true)

	res, err := s.SyncService(context.Background(), "demo", false, false)
	if err != nil {
		t.Fatalf("sync: %v", err)
	}
	if !res.Success {
		t.Fatalf("must succeed: %+v", res)
	}
	if res.Suppressed == 0 {
		t.Fatalf("suppressed must be reported: %+v", res)
	}
	if _, ok := fr.find(addrB); ok {
		t.Fatal("stale managed entry must be removed even when desired is covered by foreign entries")
	}
	if e, ok := fr.find("5.5.5.0/24"); !ok || e.Comment != "Manual" {
		t.Fatalf("foreign entry must survive: ok=%v e=%+v", ok, e)
	}
}

// Полное перекрытие и полное опустошение собственного набора: в covered-случае
// чужая включённая запись держит диапазон, поэтому полное удаление своих
// записей допустимо и синк остаётся успешным.
func TestFullWipeOfCoveredSetIsFatalOnlyWhenNotCovered(t *testing.T) {
	fr := newFakeRouter(
		addresslist.Entry{ID: "*90", Address: "5.5.5.0/24", List: "TO-VPN", Comment: "Manual", Disabled: "false"},
		addresslist.Entry{ID: "*91", Address: addrB, List: "TO-VPN", Comment: "AUTO:demo", Disabled: "false"},
	)
	src := &fakeSource{}
	src.set([]string{"5.5.5.0/24"}, nil)
	s := newE2ESyncer(t, fr, src, true)

	res, err := s.SyncService(context.Background(), "demo", false, false)
	if err != nil {
		t.Fatalf("covered full wipe must not fail: %v", err)
	}
	if !res.Success {
		t.Fatalf("must succeed: %+v", res)
	}
	if _, ok := fr.find(addrB); ok {
		t.Fatal("stale entry must be gone: range is covered by the foreign entry")
	}
}

// Параллельные SyncService для одного сервиса не должны выполнять работу дважды.
func TestConcurrentSyncOnlyOneRuns(t *testing.T) {
	fr := newFakeRouter()
	src := &fakeSource{}
	src.set([]string{addrA}, nil)
	s := newE2ESyncer(t, fr, src, true)

	var wg sync.WaitGroup
	errs := make([]error, 4)
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, errs[i] = s.SyncService(context.Background(), "demo", false, false)
		}(i)
	}
	wg.Wait()

	busy := 0
	for _, err := range errs {
		if errors.Is(err, ErrSyncBusy) {
			busy++
		} else if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	}
	if busy != 3 {
		t.Fatalf("want exactly 3 busy rejections, got %d (%v)", busy, errs)
	}
}
