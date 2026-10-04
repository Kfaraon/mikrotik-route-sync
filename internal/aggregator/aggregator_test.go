package aggregator

import (
	"net/netip"
	"testing"
)

func prefixes(t *testing.T, ss ...string) []netip.Prefix {
	t.Helper()
	out := make([]netip.Prefix, 0, len(ss))
	for _, s := range ss {
		p, err := netip.ParsePrefix(s)
		if err != nil {
			t.Fatalf("bad prefix %q: %v", s, err)
		}
		out = append(out, p.Masked())
	}
	return out
}

func hasPrefix(set []netip.Prefix, want string) bool {
	w := netip.MustParsePrefix(want)
	for _, p := range set {
		if p == w {
			return true
		}
	}
	return false
}

func TestAggregateMergesSiblings(t *testing.T) {
	in := prefixes(t, "8.8.8.0/25", "8.8.8.128/25")
	out, err := Aggregate(in)
	if err != nil {
		t.Fatalf("aggregate: %v", err)
	}
	if len(out) != 1 || out[0].String() != "8.8.8.0/24" {
		t.Fatalf("expected single 8.8.8.0/24, got %v", out)
	}
}

func TestAggregateRemovesContained(t *testing.T) {
	in := prefixes(t, "8.8.0.0/16", "8.8.5.0/24")
	out, err := Aggregate(in)
	if err != nil {
		t.Fatalf("aggregate: %v", err)
	}
	if len(out) != 1 || out[0].String() != "8.8.0.0/16" {
		t.Fatalf("expected single 8.8.0.0/16, got %v", out)
	}
}

func TestAggregateNoMergeForNonSiblings(t *testing.T) {
	in := prefixes(t, "8.8.8.0/24", "8.8.10.0/24")
	out, err := Aggregate(in)
	if err != nil {
		t.Fatalf("aggregate: %v", err)
	}
	if len(out) != 2 {
		t.Fatalf("non-sibling /24 must not merge, got %v", out)
	}
}

func TestAggregateInvariantCoverage(t *testing.T) {
	in := prefixes(t, "8.8.8.0/24", "8.8.9.0/24")
	out, err := Aggregate(in)
	if err != nil {
		t.Fatalf("aggregate: %v", err)
	}
	for _, src := range in {
		if !coversPrefix(out, src) {
			t.Fatalf("input %s not covered by result %v", src, out)
		}
	}
}

func TestValidateRejectsPrivate(t *testing.T) {
	if err := Validate(netip.MustParsePrefix("8.8.8.8/32"), nil); err != nil {
		t.Logf("host route: %v", err)
	}
	if err := Validate(netip.MustParsePrefix("10.0.0.0/8"), nil); err == nil {
		t.Fatal("expected reserved/private prefix to be rejected")
	}
}

func TestSumAddressesConservation(t *testing.T) {
	before := sumAddresses(prefixes(t, "8.8.8.0/24", "8.8.9.0/24"))
	after := sumAddresses(prefixes(t, "8.8.8.0/23"))
	if before.Cmp(after) != 0 {
		t.Fatalf("address sum not conserved: %v != %v", before, after)
	}
}

func TestIPv6IsRejected(t *testing.T) {
	if err := Validate(netip.MustParsePrefix("2001:db8::/32"), nil); err == nil {
		t.Fatal("IPv6 must be rejected (project is IPv4-only)")
	}
	out, err := Aggregate(prefixes(t, "2001:db8::/32"))
	if err != nil {
		t.Fatalf("aggregate: %v", err)
	}
	if len(out) != 0 {
		t.Fatalf("aggregate must drop IPv6, got %v", out)
	}
}

func TestSubtractPrefixDisjointAndEqual(t *testing.T) {
	// Разрыв CIDR-блоков: вычитание нечего.
	got := SubtractPrefix(netip.MustParsePrefix("1.2.3.0/24"), netip.MustParsePrefix("5.6.7.0/24"))
	if len(got) != 1 || got[0].String() != "1.2.3.0/24" {
		t.Fatalf("disjoint: expected [1.2.3.0/24], got %v", got)
	}
	// Выключаемое содержится целиком (равенство): p исчезает.
	if got := SubtractPrefix(netip.MustParsePrefix("1.2.3.0/24"), netip.MustParsePrefix("1.2.3.0/24")); got != nil {
		t.Fatalf("equal: expected nil, got %v", got)
	}
	// x шире p: p полностью внутри x.
	if got := SubtractPrefix(netip.MustParsePrefix("1.2.3.0/24"), netip.MustParsePrefix("1.2.0.0/16")); got != nil {
		t.Fatalf("superset: expected nil, got %v", got)
	}
	// Не-IPv4 не поддерживается.
	if got := SubtractPrefix(netip.MustParsePrefix("2001:db8::/32"), netip.MustParsePrefix("2001:db8::/48")); got != nil {
		t.Fatalf("IPv6: expected nil, got %v", got)
	}
}

func TestSubtractPrefixLargerBlock(t *testing.T) {
	got := SubtractPrefix(netip.MustParsePrefix("1.2.0.0/23"), netip.MustParsePrefix("1.2.1.0/24"))
	if len(got) != 1 || got[0].String() != "1.2.0.0/24" {
		t.Fatalf("/23 minus second /24 must be [1.2.0.0/24], got %v", got)
	}
}

func TestSubtractPrefixHostFromNetwork(t *testing.T) {
	p := netip.MustParsePrefix("1.2.3.0/24")
	x := netip.MustParsePrefix("1.2.3.4/32")
	got := SubtractPrefix(p, x)
	if len(got) == 0 {
		t.Fatal("expected remainder pieces")
	}

	// Сумма адресов: 256 - 1 = 255 (PROMPT III.4 — вычитание не меняет
	// множество молча: union(результат) ∪ x == p).
	if sum := sumAddresses(got); sum.Int64() != 255 {
		t.Fatalf("expected 255 addresses, got %s", sum)
	}
	// Исключённый хост не покрыт ни одним куском.
	excluded := netip.MustParseAddr("1.2.3.4")
	for _, piece := range got {
		if piece.Contains(excluded) {
			t.Fatalf("excluded host covered by %s", piece)
		}
		// Каждый кусок лежит внутри исходного блока.
		if !p.Contains(piece.Addr()) || piece.Bits() < p.Bits() {
			t.Fatalf("piece %s escapes %s", piece, p)
		}
	}
	// Результат + исключённое покрывают исходный блок целиком.
	full := append(append([]netip.Prefix{}, got...), x)
	covered := sumAddresses(full)
	if covered.Int64() != 256 {
		t.Fatalf("union(result, x) must equal p: got %s addresses", covered)
	}
	// Повторное вычитание того же хоста идемпотентно.
	again := make([]netip.Prefix, 0, len(got))
	for _, piece := range got {
		again = append(again, SubtractPrefix(piece, x)...)
	}
	if sumAddresses(again).Int64() != 255 {
		t.Fatalf("re-subtract must be idempotent: %s", sumAddresses(again))
	}
}
