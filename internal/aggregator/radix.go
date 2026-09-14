package aggregator

import (
	"fmt"
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

type RadixTree struct {
	root *node
}

func New() *RadixTree {
	return &RadixTree{root: &node{}}
}

func getBit(addr netip.Addr, i int) bit {
	var b [16]byte
	if addr.Is4() {
		b4 := addr.As4()
		copy(b[12:], b4[:])
	} else {
		b = addr.As16()
	}
	byteIdx := i / 8
	bitIdx := uint(7 - (i % 8))
	if b[byteIdx]&(1<<bitIdx) != 0 {
		return bit1
	}
	return bit0
}

func (t *RadixTree) Insert(p netip.Prefix) {
	p = p.Masked()
	n := t.root
	bits := p.Bits()
	for i := 0; i < bits; i++ {
		b := getBit(p.Addr(), i)
		if n.children[b] == nil {
			n.children[b] = &node{}
		}
		n = n.children[b]
	}
	n.isLeaf = true
}

func (t *RadixTree) Collect() []netip.Prefix {
	var result []netip.Prefix
	var walk func(n *node, addr [16]byte, depth int, isIPv4 bool)
	walk = func(n *node, addr [16]byte, depth int, isIPv4 bool) {
		if n == nil {
			return
		}
		if n.isLeaf {
			var a netip.Addr
			if isIPv4 {
				a = netip.AddrFrom4([4]byte{addr[12], addr[13], addr[14], addr[15]})
			} else {
				a = netip.AddrFrom16(addr)
			}
			result = append(result, netip.PrefixFrom(a, depth))
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
				walk(n.children[b], newAddr, depth+1, isIPv4)
			}
		}
	}

	// Separate IPv4 and IPv6
	walk(t.root, [16]byte{}, 0, false)
	return result
}

func canMerge(a, b netip.Prefix) (netip.Prefix, bool) {
	if a.Bits() != b.Bits() || a.Bits() == 0 || a.Addr().Is4() != b.Addr().Is4() {
		return netip.Prefix{}, false
	}
	// Check if they differ only in the last bit of the parent
	parentBits := a.Bits() - 1
	parentA := netip.PrefixFrom(a.Addr(), parentBits).Masked()
	parentB := netip.PrefixFrom(b.Addr(), parentBits).Masked()
	if parentA == parentB {
		return parentA, true
	}
	return netip.Prefix{}, false
}

func Aggregate(prefixes []netip.Prefix) ([]string, error) {
	if len(prefixes) == 0 {
		return nil, nil
	}

	// Validate and mask all prefixes
	masked := make([]netip.Prefix, 0, len(prefixes))
	for _, p := range prefixes {
		if !p.IsValid() {
			continue
		}
		masked = append(masked, p.Masked())
	}

	if len(masked) == 0 {
		return nil, nil
	}

	// Sort by bits (longer prefixes first) then by address
	sort.Slice(masked, func(i, j int) bool {
		if masked[i].Bits() != masked[j].Bits() {
			return masked[i].Bits() > masked[j].Bits()
		}
		return masked[i].Addr().Less(masked[j].Addr())
	})

	// Build tree
	tree := New()
	for _, p := range masked {
		tree.Insert(p)
	}

	// Collect unique prefixes from tree (handles overlapping)
	collected := tree.Collect()

	// Merge adjacent networks iteratively
	merged := mergeAdjacent(collected)

	// Convert to strings
	result := make([]string, len(merged))
	for i, p := range merged {
		result[i] = p.String()
	}

	// Validate: sum of addresses should match
	originalSum := sumAddresses(masked)
	mergedSum := sumAddresses(merged)
	if originalSum.Cmp(mergedSum) != 0 {
		return nil, fmt.Errorf("aggregation validation failed: original=%s, merged=%s", originalSum.String(), mergedSum.String())
	}

	return result, nil
}

func mergeAdjacent(prefixes []netip.Prefix) []netip.Prefix {
	if len(prefixes) == 0 {
		return nil
	}

	changed := true
	current := prefixes

	for changed {
		changed = false
		// Group by prefix length
		byLen := make(map[int][]netip.Prefix)
		for _, p := range current {
			byLen[p.Bits()] = append(byLen[p.Bits()], p)
		}

		var next []netip.Prefix
		for bits, group := range byLen {
			if bits == 0 {
				next = append(next, group...)
				continue
			}
			// Sort group
			sort.Slice(group, func(i, j int) bool {
				return group[i].Addr().Less(group[j].Addr())
			})

			merged := make([]bool, len(group))
			for i := 0; i < len(group); i++ {
				if merged[i] {
					continue
				}
				if i+1 < len(group) && !merged[i+1] {
					if parent, ok := canMerge(group[i], group[i+1]); ok {
						next = append(next, parent)
						merged[i] = true
						merged[i+1] = true
						changed = true
						continue
					}
				}
				next = append(next, group[i])
			}
		}
		current = next
	}

	return current
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
