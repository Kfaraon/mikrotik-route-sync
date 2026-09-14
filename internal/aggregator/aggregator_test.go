package aggregator

import (
	"net/netip"
	"reflect"
	"testing"
)

func TestAggregateSiblingsOnly(t *testing.T) {
	in := []netip.Prefix{netip.MustParsePrefix("10.0.0.0/24"), netip.MustParsePrefix("10.0.1.0/24")}
	got, err := Aggregate(in)
	if err != nil {
		t.Fatal(err)
	}
	want := []netip.Prefix{netip.MustParsePrefix("10.0.0.0/23")}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v want %v", got, want)
	}
}
func TestAggregateDoesNotMergeAdjacentNonSiblings(t *testing.T) {
	in := []netip.Prefix{netip.MustParsePrefix("10.0.1.0/24"), netip.MustParsePrefix("10.0.2.0/24")}
	got, err := Aggregate(in)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("unsafe merge: %v", got)
	}
}
func TestCoveredRemoved(t *testing.T) {
	in := []netip.Prefix{netip.MustParsePrefix("8.8.0.0/16"), netip.MustParsePrefix("8.8.8.0/24")}
	got, err := Aggregate(in)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].String() != "8.8.0.0/16" {
		t.Fatalf("got %v", got)
	}
}
