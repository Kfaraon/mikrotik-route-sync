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
