package collectors

import (
	"net/netip"
	"testing"
)

func TestFilterPrefixes(t *testing.T) {
	in := []netip.Prefix{
		netip.MustParsePrefix("8.8.8.0/24"),
		netip.MustParsePrefix("8.8.4.0/24"),
		netip.MustParsePrefix("1.1.1.0/24"),
	}
	opts := Options{Exclude: []string{"8.8.4.0/24"}}
	out := filterPrefixes(in, opts)
	if len(out) != 2 {
		t.Fatalf("expected 2, got %v", out)
	}
	for _, p := range out {
		if p.String() == "8.8.4.0/24" {
			t.Fatal("exclude not applied")
		}
	}
}

func TestFilterIncludeOnly(t *testing.T) {
	in := []netip.Prefix{
		netip.MustParsePrefix("8.8.8.0/24"),
		netip.MustParsePrefix("1.1.1.0/24"),
	}
	out := filterPrefixes(in, Options{IncludeOnly: []string{"8.8.8.0/24"}})
	if len(out) != 1 || out[0].String() != "8.8.8.0/24" {
		t.Fatalf("include_only failed: %v", out)
	}
}

func TestRegistryUnknownMethod(t *testing.T) {
	r := NewRegistry(&Deps{})
	if _, err := r.Build("bogus", &Params{Service: "x"}); err == nil {
		t.Fatal("expected error for unknown method")
	}
}

func TestRegistryRequiresParams(t *testing.T) {
	r := NewRegistry(&Deps{})
	if _, err := r.Build("asn", &Params{Service: "x", ASN: 0}); err == nil {
		t.Fatal("asn without ASN must fail")
	}
	if _, err := r.Build("static_url", &Params{Service: "x"}); err == nil {
		t.Fatal("static_url without URL must fail")
	}
}

func TestStaticURLRejectsBadScheme(t *testing.T) {
	if _, err := NewStaticURLCollector("ftp://example/x", nil); err == nil {
		t.Fatal("only http/https allowed")
	}
	if _, err := NewStaticURLCollector("https://example/x", nil); err != nil {
		t.Fatalf("valid url rejected: %v", err)
	}
}

func TestBGpToolsLineMatch(t *testing.T) {
	cases := []struct {
		line, target string
		want         string
		ok           bool
	}{
		{"185.230.223.0/24 206924", "206924", "185.230.223.0/24", true},
		{"1.1.1.0/24 13335", "13335", "1.1.1.0/24", true},
		{"AS13335 1.1.1.0/24", "13335", "", false},       // перепутанные колонки: поле CIDR невалидно
		{"2a0c:2f07:d::/48 206924", "206924", "", false}, // IPv6 отбрасывается
		{"203.0.113.0/24 174 6939", "174", "203.0.113.0/24", true},
		{"203.0.113.0/24 174 6939", "206924", "", false},
		{"garbage", "174", "", false},
		{"", "174", "", false},
	}
	for _, c := range cases {
		got, ok := bgpToolsLineMatch(c.line, c.target)
		if ok != c.ok || got != c.want {
			t.Errorf("bgpToolsLineMatch(%q,%q) = %q,%v want %q,%v",
				c.line, c.target, got, ok, c.want, c.ok)
		}
	}
}
