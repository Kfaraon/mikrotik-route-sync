package core

import (
	"fmt"
	"net/netip"
	"strconv"

	"github.com/Kfaraon/mikrotik-route-sync/internal/mikrotik"
)

type RouteKey struct {
	CIDR, Gateway, Table string
	Distance             int
}
type Diff struct {
	Add       []RouteKey
	Remove    []mikrotik.Route
	Unchanged int
}

func keyFromExisting(r mikrotik.Route) (RouteKey, error) {
	p, e := netip.ParsePrefix(r.DstAddress)
	if e != nil {
		return RouteKey{}, e
	}
	d := 0
	if r.Distance != "" {
		d, e = strconv.Atoi(r.Distance)
		if e != nil {
			return RouteKey{}, e
		}
	}
	return RouteKey{p.Masked().String(), r.Gateway, r.RoutingTable, d}, nil
}
func ComputeDiff(desired []RouteKey, existing []mikrotik.Route) (Diff, error) {
	want := map[RouteKey]bool{}
	for _, k := range desired {
		p, e := netip.ParsePrefix(k.CIDR)
		if e != nil {
			return Diff{}, e
		}
		k.CIDR = p.Masked().String()
		want[k] = true
	}
	have := map[RouteKey]mikrotik.Route{}
	for _, r := range existing {
		k, e := keyFromExisting(r)
		if e != nil {
			return Diff{}, fmt.Errorf("existing route %q: %w", r.DstAddress, e)
		}
		have[k] = r
	}
	var d Diff
	for k := range want {
		if _, ok := have[k]; ok {
			d.Unchanged++
		} else {
			d.Add = append(d.Add, k)
		}
	}
	for k, r := range have {
		if !want[k] {
			d.Remove = append(d.Remove, r)
		}
	}
	return d, nil
}
