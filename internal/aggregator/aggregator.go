package aggregator

import (
	"fmt"
	"math/big"
	"net/netip"
	"sort"
)

func Aggregate(prefixes []netip.Prefix) ([]netip.Prefix, error) {
	set := map[netip.Prefix]struct{}{}
	for _, p := range prefixes {
		p = p.Masked()
		set[p] = struct{}{}
	}
	removeCovered(set)
	before := addressCount(keys(set))
	for {
		changed := false
		items := keys(set)
		for _, p := range items {
			if p.Bits() == 0 {
				continue
			}
			parent := netip.PrefixFrom(p.Addr(), p.Bits()-1).Masked()
			left, right := children(parent)
			sibling := left
			if p == left {
				sibling = right
			}
			if _, ok := set[p]; !ok {
				continue
			}
			if _, ok := set[sibling]; ok {
				delete(set, p)
				delete(set, sibling)
				set[parent] = struct{}{}
				changed = true
			}
		}
		if !changed {
			break
		}
		removeCovered(set)
	}
	out := keys(set)
	if before.Cmp(addressCount(out)) != 0 {
		return nil, fmt.Errorf("aggregation invariant violated: address count changed")
	}
	sort.Slice(out, func(i, j int) bool {
		if c := out[i].Addr().Compare(out[j].Addr()); c != 0 {
			return c < 0
		}
		return out[i].Bits() < out[j].Bits()
	})
	return out, nil
}
func AggregateStrings(in []string) ([]string, error) {
	ps := make([]netip.Prefix, 0, len(in))
	for _, s := range in {
		p, e := netip.ParsePrefix(s)
		if e != nil {
			return nil, e
		}
		ps = append(ps, p.Masked())
	}
	out, e := Aggregate(ps)
	if e != nil {
		return nil, e
	}
	s := make([]string, len(out))
	for i, p := range out {
		s[i] = p.String()
	}
	return s, nil
}
func removeCovered(set map[netip.Prefix]struct{}) {
	items := keys(set)
	for _, p := range items {
		for bits := p.Bits() - 1; bits >= 0; bits-- {
			parent := netip.PrefixFrom(p.Addr(), bits).Masked()
			if _, ok := set[parent]; ok {
				delete(set, p)
				break
			}
		}
	}
}
func keys(set map[netip.Prefix]struct{}) []netip.Prefix {
	out := make([]netip.Prefix, 0, len(set))
	for p := range set {
		out = append(out, p)
	}
	return out
}
func children(parent netip.Prefix) (netip.Prefix, netip.Prefix) {
	bits := parent.Bits() + 1
	left := netip.PrefixFrom(parent.Addr(), bits).Masked()
	addr := left.Addr()
	host := 32 - bits
	if addr.Is6() {
		host = 128 - bits
	}
	step := new(big.Int).Lsh(big.NewInt(1), uint(host))
	n := addrToInt(addr)
	n.Add(n, step)
	right := netip.PrefixFrom(intToAddr(n, addr.Is4()), bits).Masked()
	return left, right
}
func addressCount(in []netip.Prefix) *big.Int {
	sum := big.NewInt(0)
	for _, p := range in {
		bits := 128 - p.Bits()
		if p.Addr().Is4() {
			bits = 32 - p.Bits()
		}
		sum.Add(sum, new(big.Int).Lsh(big.NewInt(1), uint(bits)))
	}
	return sum
}
func addrToInt(a netip.Addr) *big.Int {
	if a.Is4() {
		x := a.As4()
		return new(big.Int).SetBytes(x[:])
	}
	x := a.As16()
	return new(big.Int).SetBytes(x[:])
}
func intToAddr(n *big.Int, v4 bool) netip.Addr {
	if v4 {
		var x [4]byte
		b := n.Bytes()
		copy(x[4-len(b):], b)
		return netip.AddrFrom4(x)
	}
	var x [16]byte
	b := n.Bytes()
	copy(x[16-len(b):], b)
	return netip.AddrFrom16(x)
}
