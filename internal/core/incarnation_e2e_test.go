package core

import (
	"context"
	"testing"

	"github.com/Kfaraon/mikrotik-route-sync/internal/config"
)

// TestE2EIncarnationIsolation — полный сквозной сценарий на офлайн-синке:
// добавить -> синк (запись истории с текущим id) -> удалить -> добавить ->
// история/логи прежнего воплощения не относятся к новому.
func TestE2EIncarnationIsolation(t *testing.T) {
	fr := newFakeRouter()
	src := &fakeSource{}
	src.set([]string{addrA}, nil)

	// Сервис demo уже в конфиге; id ему не выдан (эмулируем «до фичи»).
	s := newE2ESyncer(t, fr, src, true)

	if _, err := s.SyncService(context.Background(), "demo", false, false); err != nil {
		t.Fatalf("sync: %v", err)
	}
	firstID := s.ServiceID("demo")
	// Legacy-сервис без id: запись истории без service_id.
	if firstID != "" {
		t.Fatalf("pre-existing service must have no id, got %q", firstID)
	}
	rec, ok := s.History().LastByID("demo", "")
	if !ok {
		t.Fatal("legacy history record not found")
	}

	// Удаляем и добавляем заново: новое воплощение получает свежий id.
	if err := s.RemoveService(context.Background(), "demo", false); err != nil {
		t.Fatalf("remove: %v", err)
	}
	if err := s.AddServiceWithConfig(context.Background(), "demo",
		config.ServiceOverride{Method: "static_url", StaticURL: "http://source.invalid/list.txt"}); err != nil {
		t.Fatalf("re-add: %v", err)
	}
	newID := s.ServiceID("demo")
	if newID == "" {
		t.Fatal("re-added service must get an id")
	}

	// История прежнего воплощения (без id) не должна попадать под новый id.
	if _, ok := s.History().LastByID("demo", newID); ok {
		t.Fatalf("old incarnation leaked into new id %q (last=%+v)", newID, rec)
	}

	// Синк нового воплощения: запись уже с новым id.
	if _, err := s.SyncService(context.Background(), "demo", false, false); err != nil {
		t.Fatalf("sync new incarnation: %v", err)
	}
	got, ok := s.History().LastByID("demo", newID)
	if !ok {
		t.Fatal("new incarnation history record missing")
	}
	if got.ServiceID != newID {
		t.Fatalf("record id mismatch: %q != %q", got.ServiceID, newID)
	}
}
