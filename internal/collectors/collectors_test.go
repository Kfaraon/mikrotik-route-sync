package collectors

import (
	"context"
	"net/netip"
	"sort"
	"strings"
	"testing"
	"time"
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

// H3: include_only должен пропускать вложенные префиксы (семантика та же,
// что у валидатора через aggregator.ContainsPrefix), а не только точные.
func TestFilterIncludeOnlyNested(t *testing.T) {
	in := []netip.Prefix{
		netip.MustParsePrefix("1.2.3.0/25"), // вложен в 1.2.3.0/24
		netip.MustParsePrefix("1.2.3.0/24"), // равен включению
		netip.MustParsePrefix("1.2.4.0/24"), // вне включения
	}
	out := filterPrefixes(in, Options{IncludeOnly: []string{"1.2.3.0/24"}})
	if len(out) != 2 {
		t.Fatalf("nested include_only: want 2, got %v", out)
	}
	for _, p := range out {
		if p.String() == "1.2.4.0/24" {
			t.Fatalf("out-of-include prefix kept: %v", out)
		}
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
	if _, err := r.Build("cdn", &Params{Service: "x", ASN: 0}); err == nil {
		t.Fatal("cdn without ASN must fail (ASN is required)")
	} else if got := err.Error(); !strings.Contains(got, "ASN is required") {
		t.Fatalf("wrong cdn error: %q", got)
	}
	if _, err := r.Build("whois", &Params{Service: "x"}); err == nil {
		t.Fatal("whois without domains/IPs must fail")
	}
	if _, err := r.Build("dynamic", &Params{Service: "x"}); err == nil {
		t.Fatal("dynamic without domains must fail")
	}
	if _, err := r.Build("static_url", &Params{Service: "x"}); err == nil {
		t.Fatal("static_url without URL must fail")
	}
}

func TestRegistryMethods(t *testing.T) {
	r := NewRegistry(&Deps{})
	got := append([]string(nil), r.Methods()...)
	sort.Strings(got)
	want := []string{"asn", "cdn", "dynamic", "static_url", "whois"}
	if len(got) != len(want) {
		t.Fatalf("methods=%v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("methods=%v, want %v", got, want)
		}
	}
}

func TestCDNSourceKnownASNs(t *testing.T) {
	cases := []struct {
		asn      int
		wantName string
		wantURL  string
	}{
		{13335, "Cloudflare", "https://www.cloudflare.com/ips-v4"},
		{16509, "AWS CloudFront", "https://ip-ranges.amazonaws.com/ip-ranges.json"},
		{15169, "Google", "https://www.gstatic.com/ipranges/goog.json"},
		{54113, "Fastly", "https://api.fastly.com/public-ip-list"}, // AS54113, не AS54825 (Packet Host)
	}
	for _, c := range cases {
		name, url, ok := CDNSource(c.asn)
		if !ok || name != c.wantName || url != c.wantURL {
			t.Errorf("CDNSource(%d) = %q, %q, %v; want %q, %q, true",
				c.asn, name, url, ok, c.wantName, c.wantURL)
		}
	}
	for _, asn := range []int{0, 20940, 47890, 32934} {
		if _, _, ok := CDNSource(asn); ok {
			t.Errorf("CDNSource(%d) must not match (Akamai/обычные ASN не CDN)", asn)
		}
	}
}

func TestCDNCollectorRejectsUnknownASN(t *testing.T) {
	c := NewCDNCollector(47890, nil)
	if c.Name() != "cdn" {
		t.Fatalf("name=%q, want cdn", c.Name())
	}
	if _, err := c.Collect(context.Background(), "rutor.info", Options{}); err == nil {
		t.Fatal("cdn for non-CDN ASN must fail")
	} else if got := err.Error(); !strings.Contains(got, "no CDN source") {
		t.Fatalf("wrong error: %q", got)
	}
}

func TestParsePrefixLines(t *testing.T) {
	in := []string{
		"1.1.1.0/24",
		"1.1.1.0/24",  // дубль
		"10.0.0.1/24", // немаскированный адрес — нормализуется
		"garbage",
		"",
		"2a0c:2f07:d::/48", // IPv6 отбрасывается
		"999.1.1.0/24",
	}
	out := parsePrefixLines(in)
	if len(out) != 2 {
		t.Fatalf("got %v, want 2 prefixes", out)
	}
	if out[0].String() != "1.1.1.0/24" || out[1].String() != "10.0.0.0/24" {
		t.Fatalf("wrong result: %v", out)
	}
}

// fakeResolver — офлайн-заглушка ResolverAPI для dynamic/whois тестов.
type fakeResolver struct {
	domains map[string][]string
	errOn   map[string]bool
}

func (f *fakeResolver) ResolveDomain(_ context.Context, d string) ([]string, error) {
	if f.errOn[d] {
		return nil, context.DeadlineExceeded
	}
	return f.domains[d], nil
}

func (f *fakeResolver) ASNByIP(_ context.Context, _ string) (int, error) { return 0, nil }

func TestDynamicCollectorResolvesDomains(t *testing.T) {
	r := &fakeResolver{domains: map[string][]string{
		"a.net": {"1.2.3.4", "2a00::1"}, // IPv6 отбрасывается
		"b.net": {"5.6.7.8"},
	}}
	c := NewDynamicCollector(r, nil, []string{"a.net", "b.net", "dead.net"}, nil, false)
	if c.Name() != "dynamic" {
		t.Fatalf("name=%q, want dynamic", c.Name())
	}
	res, err := c.Collect(context.Background(), "x", Options{})
	if err != nil {
		t.Fatalf("collect: %v", err)
	}
	if len(res.Prefixes) != 2 {
		t.Fatalf("prefixes=%v, want 2x /32", res.Prefixes)
	}
	for _, p := range res.Prefixes {
		if p.Bits() != 32 {
			t.Fatalf("not a host route: %v", p)
		}
	}
}

func TestDynamicCollectorEmptyFails(t *testing.T) {
	r := &fakeResolver{domains: map[string][]string{}, errOn: map[string]bool{"x.net": true}}
	c := NewDynamicCollector(r, nil, []string{"x.net"}, nil, false)
	if _, err := c.Collect(context.Background(), "x", Options{}); err == nil {
		t.Fatal("unresolvable domains must fail")
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

func TestDynamicCollectorGoogleFlagWithoutHTTP(t *testing.T) {
	r := &fakeResolver{domains: map[string][]string{"g.net": {"8.8.8.8"}}}
	// includeGoogle без HTTP-клиента: диапазоны пропустить, DNS-результат отдать.
	c := NewDynamicCollector(r, nil, []string{"g.net"}, nil, true)
	res, err := c.Collect(context.Background(), "google", Options{})
	if err != nil {
		t.Fatalf("collect: %v", err)
	}
	if len(res.Prefixes) != 1 || res.Prefixes[0].String() != "8.8.8.8/32" {
		t.Fatalf("prefixes=%v", res.Prefixes)
	}
}

// fakePrefixCache — офлайн-кэш префиксов по ASN.
type fakePrefixCache struct {
	data map[int][]string
}

func (f *fakePrefixCache) GetPrefixes(asn int) ([]string, bool) {
	p, ok := f.data[asn]
	return p, ok
}

func (f *fakePrefixCache) SetPrefixes(asn int, prefixes []string, _ time.Duration) error {
	if f.data == nil {
		f.data = map[int][]string{}
	}
	f.data[asn] = prefixes
	return nil
}

type fakeASNResolver struct {
	fakeResolver
	asn int
}

func (f *fakeASNResolver) ASNByIP(_ context.Context, _ string) (int, error) {
	return f.asn, nil
}

func TestWHOISAlwaysCoversResolvedIPs(t *testing.T) {
	// Кэш отдаёт 1 широкий префикс, не покрывающий IP сервиса;
	// кап maxPrefixes=1 отрезал бы всё — гарантия добавляет /32.
	r := &fakeASNResolver{
		fakeResolver: fakeResolver{domains: map[string][]string{
			"x.net": {"10.9.9.9"},
		}},
		asn: 66666,
	}
	cache := &fakePrefixCache{data: map[int][]string{66666: {"11.0.0.0/8"}}}
	c := NewWHOISCollector(r, &HTTP{}, cache, 0, []string{"x.net"}, nil, 1)
	res, err := c.Collect(context.Background(), "x", Options{})
	if err != nil {
		t.Fatalf("collect: %v", err)
	}
	foundHost, foundNet := false, false
	for _, p := range res.Prefixes {
		switch p.String() {
		case "10.9.9.9/32":
			foundHost = true
		case "11.0.0.0/8":
			foundNet = true
		}
	}
	if !foundHost || !foundNet {
		t.Fatalf("prefixes=%v, want host /32 plus cached set", res.Prefixes)
	}
}
