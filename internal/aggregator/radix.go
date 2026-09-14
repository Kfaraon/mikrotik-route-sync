package aggregator

import (
	"fmt"
	"net/netip"
	"sort"
	"strings"
)

type bit int
const ( bit0 bit = 0; bit1 bit = 1 )

type trieNode struct {
	children [2]*trieNode
	terminal bool
	depth    int
	prefix   netip.Prefix
}

type RadixTree struct { root *trieNode }

func NewRadixTree() *RadixTree {
	return &RadixTree{root: &trieNode{}}
}

func (t *RadixTree) Insert(p netip.Prefix) {
	p = p.Masked()
	node := t.root
	addr := p.Addr()
	bits := p.Bits()
	for i := 0; i < bits; i++ {
		b := getBit(addr, i)
		if node.children[b] == nil {
			node.children[b] = &trieNode{depth: i + 1}
		}
		node = node.children[b]
	}
	node.terminal = true
	node.prefix = p
}

func getBit(addr netip.Addr, i int) bit {
	if addr.Is4() {
		b := addr.As4()
		byteIdx := i / 8
		bitIdx := uint(7 - (i % 8))
		if b[byteIdx]&(1<<bitIdx) != 0 { return bit1 }
		return bit0
	}
	b := addr.As16()
	byteIdx := i / 8
	bitIdx := uint(7 - (i % 8))
	if b[byteIdx]&(1<<bitIdx) != 0 { return bit1 }
	return bit0
}

// Compact удаляет терминальные узлы, если родитель тоже терминальный
// (подсеть полностью покрыта более широкой подсетью)
func (t *RadixTree) Compact() {
	t.compact(t.root, false)
}

func (t *RadixTree) compact(node *trieNode, parentTerminal bool) bool {
	if node == nil { return false }
	hasChildren := false
	for _, c := range node.children {
		if c != nil {
			hasChildren = true
			t.compact(c, parentTerminal || node.terminal)
		}
	}
	if parentTerminal && node.terminal {
		node.terminal = false
	}
	if node.terminal { return true }
	return hasChildren
}

// Merge объединяет смежные подсети с одинаковой длиной маски
func (t *RadixTree) Merge() {
	changed := true
	for changed {
		changed = false
		merged := NewRadixTree()
		prefixes := t.Collect()
		sort.Slice(prefixes, func(i, j int) bool {
			if prefixes[i].Bits() != prefixes[j].Bits() {
				return prefixes[i].Bits() > prefixes[j].Bits()
			}
			return prefixes[i].Addr().Compare(prefixes[j].Addr()) < 0
		})
		used := make([]bool, len(prefixes))
		for i := 0; i < len(prefixes); i++ {
			if used[i] { continue }
			for j := i + 1; j < len(prefixes); j++ {
				if used[j] { continue }
				if merged, ok := canMerge(prefixes[i], prefixes[j]); ok {
					merged.Insert(merged.prefix)
					used[i], used[j] = true, true
					changed = true
					break
				}
			}
			if !used[i] { merged.Insert(prefixes[i]) }
		}
		*t = *merged
	}
}

type mergeResult struct {
	prefix netip.Prefix
	ok     bool
}

func canMerge(a, b netip.Prefix) (mergeResult, bool) {
	if a.Bits() != b.Bits() || a.Bits() == 0 || a.Addr().Is4() != b.Addr().Is4() {
		return mergeResult{}, false
	}
	parentA := netip.PrefixFrom(a.Addr().Prev(), a.Bits()-1).Masked()
	parentB := netip.PrefixFrom(b.Addr().Prev(), b.Bits()-1).Masked()
	if parentA == parentB {
		return mergeResult{prefix: parentA, ok: true}, true
	}
	return mergeResult{}, false
}

func (t *RadixTree) Collect() []netip.Prefix {
	var result []netip.Prefix
	t.collect(t.root, netip.Addr{}, 0, &result)
	return result
}

func (t *RadixTree) collect(node *trieNode, addr netip.Addr, depth int, result *[]netip.Prefix) {
	if node == nil { return }
	if node.terminal && node.prefix.IsValid() {
		*result = append(*result, node.prefix)
		return
	}
	// Рекурсия в дочерние узлы
	for b, child := range node.children {
		if child != nil {
			// Восстановление адреса по битам — упрощённо через prefix
			_ = b
			t.collect(child, addr, depth+1, result)
		}
	}
}

// Aggregate — главная функция агрегации с проверкой суммы адресов
func Aggregate(prefixes []string) ([]string, error) {
	if len(prefixes) == 0 { return nil, nil }

	// Парсинг и дедупликация
	seen := make(map[netip.Prefix]struct{})
	var list []netip.Prefix
	for _, s := range prefixes {
		p, err := netip.ParsePrefix(strings.TrimSpace(s))
		if err != nil { return nil, fmt.Errorf("bad prefix %q: %w", s, err) }
		p = p.Masked()
		if _, ok := seen[p]; !ok {
			seen[p] = struct{}{}
			list = append(list, p)
		}
	}
	if len(list) == 0 { return nil, nil }

	// Подсчёт суммы адресов ДО
	sumBefore := sumAddresses(list)

	// Radix Tree
	tree := NewRadixTree()
	for _, p := range list { tree.Insert(p) }
	tree.Compact()
	tree.Merge()
	aggregated := tree.Collect()

	// Подсчёт суммы адресов ПОСЛЕ
	sumAfter := sumAddresses(aggregated)

	// Валидация
	if sumBefore != sumAfter {
		return nil, fmt.Errorf("aggregation validation failed: before=%d after=%d", sumBefore, sumAfter)
	}

	out := make([]string, len(aggregated))
	for i, p := range aggregated { out[i] = p.String() }
	return out, nil
}

func sumAddresses(prefixes []netip.Prefix) uint64 {
	var sum uint64
	for _, p := range prefixes {
		bits := p.Bits()
		if bits < 0 || bits > 128 { continue }
		size := uint64(1) << uint(32-bits)
		if p.Addr().Is6() { size = uint64(1) << uint(128-bits) }
		sum += size
	}
	return sum
}
