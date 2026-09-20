package core

import (
	"testing"

	"github.com/Kfaraon/mikrotik-route-sync/internal/mikrotik"
)

func TestComputeDiffIdempotent(t *testing.T) {
	desired := []RouteKey{
		{CIDR: "8.8.8.0/24", Gateway: "wg0", Table: "main", Distance: 2},
	}
	existing := []mikrotik.Route{
		{ID: "*1", DstAddress: "8.8.8.0/24", Gateway: "wg0", RoutingTable: "main", Distance: "2"},
	}
	d, err := ComputeDiff(desired, existing)
	if err != nil {
		t.Fatalf("diff: %v", err)
	}
	if len(d.Add) != 0 || len(d.Remove) != 0 || d.Unchanged != 1 {
		t.Fatalf("expected idempotent diff, got add=%d remove=%d unchanged=%d", len(d.Add), len(d.Remove), d.Unchanged)
	}
}

func TestComputeDiffNormalizesHostBits(t *testing.T) {
	desired := []RouteKey{
		{CIDR: "8.8.8.1/24", Gateway: "wg0", Table: "main", Distance: 2},
	}
	existing := []mikrotik.Route{
		{ID: "*1", DstAddress: "8.8.8.0/24", Gateway: "wg0", RoutingTable: "main", Distance: "2"},
	}
	d, err := ComputeDiff(desired, existing)
	if err != nil {
		t.Fatalf("diff: %v", err)
	}
	if d.Unchanged != 1 || len(d.Add) != 0 {
		t.Fatalf("host bits must normalize away, got %+v", d)
	}
}

func TestComputeDiffAddRemove(t *testing.T) {
	desired := []RouteKey{
		{CIDR: "1.1.1.0/24", Gateway: "wg0", Table: "main", Distance: 2},
		{CIDR: "2.2.2.0/24", Gateway: "wg0", Table: "main", Distance: 2},
	}
	existing := []mikrotik.Route{
		{ID: "*1", DstAddress: "2.2.2.0/24", Gateway: "wg0", RoutingTable: "main", Distance: "2"},
		{ID: "*2", DstAddress: "3.3.3.0/24", Gateway: "wg0", RoutingTable: "main", Distance: "2"},
	}
	d, err := ComputeDiff(desired, existing)
	if err != nil {
		t.Fatalf("diff: %v", err)
	}
	if len(d.Add) != 1 || d.Add[0].CIDR != "1.1.1.0/24" {
		t.Fatalf("bad add: %+v", d.Add)
	}
	if len(d.Remove) != 1 || d.Remove[0].ID != "*2" {
		t.Fatalf("bad remove: %+v", d.Remove)
	}
	if d.Unchanged != 1 {
		t.Fatalf("bad unchanged: %d", d.Unchanged)
	}
}

func TestComputeDiffSkipsBrokenExisting(t *testing.T) {
	desired := []RouteKey{{CIDR: "1.1.1.0/24", Gateway: "wg0", Table: "main", Distance: 2}}
	existing := []mikrotik.Route{
		{ID: "*1", DstAddress: "not-a-cidr", Gateway: "wg0", RoutingTable: "main", Distance: "2"},
	}
	d, err := ComputeDiff(desired, existing)
	if err != nil {
		t.Fatalf("diff: %v", err)
	}
	if len(d.Remove) != 0 {
		t.Fatalf("broken existing must be skipped (fail-closed), got %+v", d.Remove)
	}
}
