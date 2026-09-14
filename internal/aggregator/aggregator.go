package aggregator

import (
	"fmt"
	"net/netip"
	"sort"
)

func Aggregate(prefixes []string) ([]string, error) {
	var list []netip.Prefix
	seen := make(map[netip.Prefix]struct{})
	for _, s := range prefixes {
		p, err := netip.ParsePrefix(s)
		if err != nil { return nil, fmt.Errorf("bad prefix %q: %w", s, err) }
		p = p.Masked()
		if _, ok := seen[p]; !ok { seen[p] = struct{}{}; list = append(list, p) }
	}
	if len(list) == 0 { return nil, nil }

	sort.Slice(list, func(i, j int) bool {
		if c := list[i].Addr().Compare(list[j].Addr()); c != 0 { return c < 0 }
		return list[i].Bits() < list[j].Bits()
	})

	filtered := list[:0]
	for _, p := range list {
		if len(filtered) == 0 { filtered = append(filtered, p); continue }
		last := filtered[len(filtered)-1]
		if last.Contains(p.Addr()) && last.Bits() <= p.Bits() { continue }
		filtered = append(filtered, p)
	}
	list = filtered

	changed := true
	for changed {
		changed = false
		var merged []netip.Prefix
		i := 0
		for i < len(list) {
			if i+1 < len(list) && canMerge(list[i], list[i+1]) {
				merged = append(merged, merge(list[i]))
				i += 2; changed = true
			} else {
				merged = append(merged, list[i])
				i++
			}
		}
		list = merged
	}

	out := make([]string, len(list))
	for i, p := range list { out[i] = p.String() }
	return out, nil
}

func canMerge(a, b netip.Prefix) bool {
	if a.Bits() != b.Bits() || a.Bits() == 0 || a.Addr().Is4() != b.Addr().Is4() { return false }
	parentA := netip.PrefixFrom(a.Addr().Prev(), a.Bits()-1)
	return parentA.Contains(b.Addr())
}

func merge(a netip.Prefix) netip.Prefix { return netip.PrefixFrom(a.Addr().Prev(), a.Bits()-1) }