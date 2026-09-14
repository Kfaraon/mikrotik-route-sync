package validator

import (
	"fmt"
	"net/netip"
	"sort"
)

func Prefixes(in []string, exclude []string) ([]netip.Prefix, error) {
	ex := map[string]bool{}
	for _, s := range exclude {
		if p, e := netip.ParsePrefix(s); e == nil {
			ex[p.Masked().String()] = true
		}
	}
	seen := map[netip.Prefix]struct{}{}
	out := make([]netip.Prefix, 0, len(in))
	for _, s := range in {
		p, err := netip.ParsePrefix(s)
		if err != nil {
			if a, e := netip.ParseAddr(s); e == nil {
				if a.Is4() {
					p = netip.PrefixFrom(a, 32)
				} else {
					p = netip.PrefixFrom(a, 128)
				}
			} else {
				return nil, fmt.Errorf("invalid prefix %q", s)
			}
		}
		p = p.Masked()
		if ex[p.String()] {
			continue
		}
		if reject(p) {
			continue
		}
		if _, ok := seen[p]; ok {
			continue
		}
		seen[p] = struct{}{}
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool {
		if c := out[i].Addr().Compare(out[j].Addr()); c != 0 {
			return c < 0
		}
		return out[i].Bits() < out[j].Bits()
	})
	return out, nil
}
func reject(p netip.Prefix) bool {
	a := p.Addr()
	if !a.IsValid() || a.IsPrivate() || a.IsLoopback() || a.IsLinkLocalUnicast() || a.IsMulticast() || a.IsUnspecified() {
		return true
	}
	if a.Is4() && p.Bits() < 10 {
		return true
	}
	if a.Is6() && p.Bits() < 16 {
		return true
	}
	return false
}
