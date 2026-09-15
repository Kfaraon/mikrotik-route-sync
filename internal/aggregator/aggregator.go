package aggregator

import (
	"fmt"
	"net/netip"
	"sort"

	"github.com/Kfaraon/mikrotik-route-sync/pkg/cidrutil"
)

func Aggregate(in []netip.Prefix) ([]netip.Prefix, error) {
	in = cidrutil.RemoveContained(in)
	cur := append([]netip.Prefix(nil), in...)
	for {
		sort.Slice(cur, func(i, j int) bool {
			if cur[i].Addr().Compare(cur[j].Addr()) == 0 {
				return cur[i].Bits() < cur[j].Bits()
			}
			return cur[i].Addr().Compare(cur[j].Addr()) < 0
		})
		changed := false
		out := make([]netip.Prefix, 0, len(cur))
		for i := 0; i < len(cur); {
			if i+1 < len(cur) {
				if p, ok := siblings(cur[i], cur[i+1]); ok {
					out = append(out, p)
					i += 2
					changed = true
					continue
				}
			}
			out = append(out, cur[i])
			i++
		}
		cur = cidrutil.RemoveContained(out)
		if !changed {
			break
		}
	}
	if !coverageInvariant(in, cur) {
		return nil, fmt.Errorf("aggregation coverage invariant failed")
	}
	return cur, nil
}
func siblings(a, b netip.Prefix) (netip.Prefix, bool) {
	if a.Bits() != b.Bits() || a.Bits() == 0 || a.Addr().BitLen() != b.Addr().BitLen() {
		return netip.Prefix{}, false
	}
	parentBits := a.Bits() - 1
	pa := netip.PrefixFrom(a.Addr(), parentBits).Masked()
	pb := netip.PrefixFrom(b.Addr(), parentBits).Masked()
	if pa != pb {
		return netip.Prefix{}, false
	}
	if a == b {
		return netip.Prefix{}, false
	}
	return pa, true
}
func coverageInvariant(src, dst []netip.Prefix) bool {
	for _, s := range src {
		ok := false
		for _, d := range dst {
			if cidrutil.ContainsPrefix(d, s) {
				ok = true
				break
			}
		}
		if !ok {
			return false
		}
	}
	return true
}
