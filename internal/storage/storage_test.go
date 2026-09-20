package storage

import (
	"path/filepath"
	"testing"
	"time"
)

func openTestCache(t *testing.T) *Cache {
	t.Helper()
	c, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
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
