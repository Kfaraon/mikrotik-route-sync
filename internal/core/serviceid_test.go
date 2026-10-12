package core

import (
	"context"
	"regexp"
	"testing"

	"github.com/Kfaraon/mikrotik-route-sync/internal/config"
)

var uuidV4Re = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)

func TestNewServiceIDIsUUIDv4(t *testing.T) {
	seen := make(map[string]bool, 100)
	for range 100 {
		id := newServiceID()
		if !uuidV4Re.MatchString(id) {
			t.Fatalf("not a UUID v4: %q", id)
		}
		if seen[id] {
			t.Fatalf("duplicate id: %q", id)
		}
		seen[id] = true
	}
}

func TestAddRemoveServiceAssignsID(t *testing.T) {
	m := &mockRouter{t: t}
	s, cfg := newTestSyncer(t, m.serve(nil, false))

	const name = "example.com"
	// Метод asn не требует сети на этапе добавления.
	if err := s.AddServiceWithConfig(context.Background(), name, config.ServiceOverride{Method: "asn"}); err != nil {
		t.Fatalf("add: %v", err)
	}

	id := s.ServiceID(name)
	if id == "" {
		t.Fatal("id not assigned on add")
	}
	if !uuidV4Re.MatchString(id) {
		t.Fatalf("assigned id is not UUID v4: %q", id)
	}
	if cfg.ServiceIDs[name] != id {
		t.Fatalf("id not persisted in config: %q", cfg.ServiceIDs[name])
	}

	// Повторное добавление без удаления отклоняется; id остаётся прежним.
	if err := s.AddServiceWithConfig(context.Background(), name, config.ServiceOverride{Method: "asn"}); err == nil {
		t.Fatal("duplicate add must fail")
	}
	if s.ServiceID(name) != id {
		t.Fatal("id changed on failed add")
	}

	if err := s.RemoveService(context.Background(), name, false); err != nil {
		t.Fatalf("remove: %v", err)
	}
	if s.ServiceID(name) != "" {
		t.Fatal("id not deleted on remove")
	}

	// Повторное добавление после удаления — новое воплощение (новый id).
	if err := s.AddServiceWithConfig(context.Background(), name, config.ServiceOverride{Method: "asn"}); err != nil {
		t.Fatalf("re-add: %v", err)
	}
	if id2 := s.ServiceID(name); id2 == "" || id2 == id {
		t.Fatalf("re-add must assign a fresh id, got %q (old %q)", id2, id)
	}
}
