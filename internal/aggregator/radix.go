package aggregator

import (
	"math/big"
	"net/netip"
	"sort"
)

type bit uint8

const (
	bit0 bit = 0
	bit1 bit = 1
)

type node struct {
	children [2]*node
	isLeaf   bool
}

// RadixTree — префиксное дерево (binary trie) для агрегации IPv4.
// Агрегация IPv6 намеренно НЕ выполняется (решение проекта: только IPv4).
type RadixTree struct {
	root *node
}

func New() *RadixTree {
	return &RadixTree{root: &node{}}
}

func getBit4(addr netip.Addr, i int) bit {
	b := addr.As4()
	byteIdx := i / 8
	bitIdx := uint(7 - (i % 8))
	if b[byteIdx]&(1<<bitIdx) != 0 {
		return bit1
	}
	return bit0
}

// Insert добавляет IPv4-префикс. IPv6-префиксы игнорируются.
func (t *RadixTree) Insert(p netip.Prefix) {
	if !p.IsValid() {
		return
	}
	p = p.Masked()
	if !p.Addr().Is4() {
		return
	}
	if p.Bits() > 32 {
		return
	}

	n := t.root
	for i := 0; i < p.Bits(); i++ {
		b := getBit4(p.Addr(), i)
		if n.children[b] == nil {
			n.children[b] = &node{}
		}
		n = n.children[b]
	}
	n.isLeaf = true
}

// full возвращает true, если поддерево полностью покрывает узел
// (сам узел — лист или оба ребёнка полные).
func (n *node) full() bool {
	if n == nil {
		return false
	}
	if n.isLeaf {
		return true
	}
	return n.children[bit0].full() && n.children[bit1].full()
}

// Collect возвращает минимальный набор IPv4-префиксов, эквивалентный
// вставленным. Объединяются только настоящие sibling-поддеревья.
func (t *RadixTree) Collect() []netip.Prefix {
	var result []netip.Prefix
	var walk func(n *node, addr [4]byte, depth int)
	walk = func(n *node, addr [4]byte, depth int) {
		if n == nil {
			return
		}
		if n.full() {
			result = append(result, netip.PrefixFrom(netip.AddrFrom4(addr), depth).Masked())
			return
		}
		for b := bit(0); b < 2; b++ {
			if n.children[b] != nil {
				newAddr := addr
				byteIdx := depth / 8
				bitIdx := uint(7 - (depth % 8))
				if b == bit1 {
					newAddr[byteIdx] |= 1 << bitIdx
				} else {
					newAddr[byteIdx] &^= 1 << bitIdx
				}
				walk(n.children[b], newAddr, depth+1)
			}
		}
	}

	walk(t.root, [4]byte{}, 0)

	sort.Slice(result, func(i, j int) bool {
		if result[i].Bits() != result[j].Bits() {
			return result[i].Bits() < result[j].Bits()
		}
		return result[i].Addr().Compare(result[j].Addr()) < 0
	})
	return result
}

// AggregatePrefixes — агрегация IPv4-префиксов через radix tree за O(n·L).
// IPv6-префиксы в дереве не участвуют.
func AggregatePrefixes(in []netip.Prefix) []netip.Prefix {
	t := New()
	for _, p := range in {
		t.Insert(p)
	}
	return t.Collect()
}

func sumAddresses(prefixes []netip.Prefix) *big.Int {
	sum := big.NewInt(0)
	one := big.NewInt(1)
	for _, p := range prefixes {
		bits := p.Bits()
		if bits < 0 {
			continue
		}
		var totalBits int
		if p.Addr().Is4() {
			totalBits = 32
		} else {
			totalBits = 128
		}
		hosts := big.NewInt(0)
		hosts.Lsh(one, uint(totalBits-bits))
		sum.Add(sum, hosts)
	}
	return sum
}
