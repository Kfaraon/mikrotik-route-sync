package core

import (
	"github.com/Kfaraon/mikrotik-route-sync/internal/mikrotik"
	"testing"
)

func TestDiffIdempotent(t *testing.T) {
	d, e := ComputeDiff([]RouteKey{{CIDR: "1.1.1.0/24", Gateway: "gw", Table: "main", Distance: 2}}, []mikrotik.Route{{ID: "*1", DstAddress: "1.1.1.0/24", Gateway: "gw", RoutingTable: "main", Distance: "2"}})
	if e != nil || len(d.Add) != 0 || len(d.Remove) != 0 || d.Unchanged != 1 {
		t.Fatalf("%+v %v", d, e)
	}
}
