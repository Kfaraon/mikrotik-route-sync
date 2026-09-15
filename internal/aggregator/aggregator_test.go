package aggregator

import (
	"net/netip"
	"testing"
)

func TestAggregateSiblings(t *testing.T) {
	in := []netip.Prefix{netip.MustParsePrefix("1.1.0.0/25"), netip.MustParsePrefix("1.1.0.128/25")}
	out, e := Aggregate(in)
	if e != nil || len(out) != 1 || out[0].String() != "1.1.0.0/24" {
		t.Fatalf("out=%v err=%v", out, e)
	}
}
