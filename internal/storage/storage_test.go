package storage

import (
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	bolt "go.etcd.io/bbolt"
)

func openTestCache(t *testing.T) *Cache {
	t.Helper()
	c, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

func TestSnapshotRoundTrip(t *testing.T) {
	c := openTestCache(t)
	prefixes := []string{"8.8.8.0/24", "8.8.4.0/24"}

	id, err := c.CreateSnapshot("instagram", prefixes)
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	var got []string
	if err := c.GetSnapshot("instagram", id, &got); err != nil {
		t.Fatalf("get: %v", err)
	}
	if len(got) != 2 || got[0] != prefixes[0] {
		t.Fatalf("round trip mismatch: %v", got)
	}

	infos, err := c.ListSnapshots("instagram")
	if err != nil {
		t.Fatal(err)
	}
	if len(infos) != 1 || infos[0].ID != id || infos[0].Count != 2 {
		t.Fatalf("list mismatch: %+v", infos)
	}
	if infos[0].CreatedAt.IsZero() {
		t.Fatal("created_at must be persisted")
	}

	if err := c.DeleteSnapshot("instagram", id); err != nil {
		t.Fatal(err)
	}
	infos, _ = c.ListSnapshots("instagram")
	if len(infos) != 0 {
		t.Fatalf("expected empty after delete: %+v", infos)
	}
}

func TestSnapshotIsolation(t *testing.T) {
	c := openTestCache(t)
	if _, err := c.CreateSnapshot("instagram", []string{"8.8.8.0/24"}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.CreateSnapshot("youtube", []string{"8.8.4.0/24"}); err != nil {
		t.Fatal(err)
	}
	ins, _ := c.ListSnapshots("instagram")
	if len(ins) != 1 {
		t.Fatalf("service isolation broken: %+v", ins)
	}
}

// E6/O4: метаданные снапшота читаются из отдельного бакета, без разбора
// тела. Проверяем, что создание пишет метаиндекс и ListSnapshots отдаёт
// корректные CreatedAt/Count.
func TestSnapshotMetaIndex(t *testing.T) {
	c := openTestCache(t)
	id, err := c.CreateSnapshot("svc", []string{"1.1.1.0/24", "2.2.2.0/24", "3.3.3.0/24"})
	if err != nil {
		t.Fatal(err)
	}
	infos, err := c.ListSnapshots("svc")
	if err != nil {
		t.Fatal(err)
	}
	if len(infos) != 1 || infos[0].ID != id || infos[0].Count != 3 || infos[0].CreatedAt.IsZero() {
		t.Fatalf("meta index mismatch: %+v", infos)
	}
	// Удаление снапшота убирает и метазапись (иначе список рос бы призраками).
	if err := c.DeleteSnapshot("svc", id); err != nil {
		t.Fatal(err)
	}
	if infos, _ := c.ListSnapshots("svc"); len(infos) != 0 {
		t.Fatalf("meta left after delete: %+v", infos)
	}
}

// Fallback: снапшот без метазаписи (созданный старой версией) всё равно
// попадает в список с метаданными из тела.
func TestListSnapshotsLegacyFallback(t *testing.T) {
	c := openTestCache(t)
	key := "legacy:1700000000000000000"
	body := `{"created_at":"2023-11-14T22:13:20Z","prefixes":["9.9.9.0/24"]}`
	c.mu.Lock()
	err := c.db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket(bucketSnapshots).Put([]byte(key), []byte(body))
	})
	c.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	infos, err := c.ListSnapshots("legacy")
	if err != nil {
		t.Fatal(err)
	}
	if len(infos) != 1 || infos[0].Count != 1 || infos[0].ID != "1700000000000000000" {
		t.Fatalf("legacy fallback failed: %+v", infos)
	}
}

func TestASNPrefixedCacheTTL(t *testing.T) {
	c := openTestCache(t)
	if err := c.SetASN("8.8.8.8", 15169, time.Hour); err != nil {
		t.Fatal(err)
	}
	asn, ok := c.GetASN("8.8.8.8")
	if !ok || asn != 15169 {
		t.Fatalf("cache miss: %d %v", asn, ok)
	}
	if err := c.SetASN("9.9.9.9", 20, -time.Second); err != nil {
		t.Fatal(err)
	}
	if _, ok := c.GetASN("9.9.9.9"); ok {
		t.Fatal("expired entry must not be returned")
	}
}

func TestOpenLockedDBHintsDaemon(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "locked.db")
	c, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	// Второй Open ждёт 2с (Timeout в Open) и должен объяснить причину:
	// запущенный демон держит flock.
	start := time.Now()
	_, err = Open(path)
	if err == nil {
		t.Fatal("expected lock error")
	}
	if elapsed := time.Since(start); elapsed < time.Second {
		t.Fatalf("expected ~2s lock wait, got %v", elapsed)
	}
	for _, want := range []string{"locked by another process", "stop the daemon"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error %q must mention %q", err, want)
		}
	}
}

// TestPurgeTickerRestartRace — перезапуск и остановка тикера параллельно:
// бывший data race полей purgeTicker/purgeDone (горутина читала без мьютекса).
func TestPurgeTickerRestartRace(t *testing.T) {
	c := openTestCache(t)
	defer func() { _ = c.Close() }()

	var wg sync.WaitGroup
	stop := make(chan struct{})
	for g := 0; g < 3; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 20; i++ {
				select {
				case <-stop:
					return
				default:
				}
				c.StartPurgeTicker(time.Millisecond)
			}
		}()
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 20; i++ {
			c.StopPurgeTicker()
		}
		close(stop)
	}()
	wg.Wait()
}
