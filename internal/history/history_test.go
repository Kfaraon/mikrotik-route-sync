package history

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/Kfaraon/mikrotik-route-sync/internal/storage"
)

func TestInMemoryRing(t *testing.T) {
	h := NewHistory(3)
	for i := range 10 {
		h.Add(Record{Service: "a", Added: i})
	}
	recs := h.Records("", 100)
	if len(recs) != 3 {
		t.Fatalf("ring must cap at limit: %d", len(recs))
	}
	if recs[0].Added != 9 {
		t.Fatalf("newest first expected, got %+v", recs[0])
	}
	last, ok := h.Last("a")
	if !ok || last.Added != 9 {
		t.Fatal("Last broken")
	}
}

func TestServiceFilterAndPersistence(t *testing.T) {
	c, err := storage.Open(filepath.Join(t.TempDir(), "h.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	h := NewHistory(100)
	h.AttachStore(c)
	h.Add(Record{Service: "instagram", Added: 2, Time: time.Now()})
	h.Add(Record{Service: "youtube", Removed: 1})

	if recs := h.Records("instagram", 10); len(recs) != 1 || recs[0].Added != 2 {
		t.Fatalf("filter broken: %+v", recs)
	}

	// восстановление из bbolt в новом экземпляре
	h2 := NewHistory(100)
	h2.AttachStore(c)
	if recs := h2.Records("", 10); len(recs) != 2 {
		t.Fatalf("persistence broken: %+v", recs)
	}
}
