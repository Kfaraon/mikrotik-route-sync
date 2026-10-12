package resolver

// Тесты фолбэк-цепочки DNS (resolver.go): порядок резолверов и перебор —
// на заглушках, без сети.

import (
	"context"
	"fmt"
	"net"
	"net/netip"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestWithFallbacksDefaults(t *testing.T) {
	r := NewResolver("192.168.88.88:53", nil, nil, 0)
	if len(r.fallbacks) != 2 || r.fallbacks[0] != "8.8.8.8:53" || r.fallbacks[1] != "1.1.1.1:53" {
		t.Fatalf("default fallbacks=%v", r.fallbacks)
	}
}

func TestWithFallbacksDedup(t *testing.T) {
	r := NewResolver("8.8.8.8:53", nil, nil, 0)
	r.WithFallbacks("8.8.8.8:53", " 1.1.1.1:53 ", "", "1.1.1.1:53")
	if len(r.fallbacks) != 2 || r.fallbacks[0] != "1.1.1.1:53" || r.fallbacks[1] != "1.1.1.1:53" {
		t.Fatalf("dedup/trim failed: %v", r.fallbacks)
	}
}

func TestResolversOrder(t *testing.T) {
	r := NewResolver("192.168.88.88:53", nil, nil, 0)
	r.WithFallbacks("9.9.9.9:53")
	rs := r.resolvers()
	if len(rs) != 2 || rs[0] == nil || rs[1] == nil {
		t.Fatalf("want [primary fallback], got %v", rs)
	}

	sys := NewResolver("", nil, nil, 0)
	sys.WithFallbacks() // дефолтные публичные
	rs = sys.resolvers()
	if len(rs) != 3 || rs[0] != nil {
		t.Fatalf("want [system + 2 fallbacks], got %v", rs)
	}
}

func TestLookupWithFallbackFirstSuccess(t *testing.T) {
	calls := 0
	stub := func(_ context.Context, _ *net.Resolver) (string, error) {
		calls++
		if calls < 3 {
			return "", fmt.Errorf("dns down %d", calls)
		}
		return "ok", nil
	}
	rs := []*net.Resolver{{}, {}, {}}
	got, err := lookupWithFallback(context.Background(), rs, stub)
	if err != nil || got != "ok" || calls != 3 {
		t.Fatalf("got=%q err=%v calls=%d", got, err, calls)
	}
}

func TestLookupWithFallbackAllFail(t *testing.T) {
	stub := func(_ context.Context, _ *net.Resolver) (string, error) {
		return "", fmt.Errorf("boom")
	}
	_, err := lookupWithFallback(context.Background(), []*net.Resolver{{}, {}}, stub)
	if err == nil || err.Error() != "boom" {
		t.Fatalf("want last error, got %v", err)
	}
}

func TestLookupWithFallbackContextCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	stub := func(_ context.Context, _ *net.Resolver) (string, error) {
		return "", fmt.Errorf("dns down")
	}
	_, err := lookupWithFallback(ctx, []*net.Resolver{{}, {}}, stub)
	if err != context.Canceled {
		t.Fatalf("want context.Canceled, got %v", err)
	}
}

type fakeFetcher struct {
	byIP map[string]string
	err  error
}

func (f *fakeFetcher) Fetch(_ context.Context, url string) ([]byte, error) {
	if f.err != nil {
		return nil, f.err
	}
	ip := url[strings.LastIndex(url, "/")+1:]
	if body, ok := f.byIP[ip]; ok {
		return []byte(body), nil
	}
	return []byte(`{"entities":[]}`), nil
}

func rdapWithASN(asn int) string {
	return fmt.Sprintf(`{"entities":[{"handle":"AS%d","entities":[{"id":"AUTNUM%d"}]}]}`, asn, asn)
}

func TestCollectRDAPASNs(t *testing.T) {
	v := rdapResponse{Entities: []rdapEntity{
		{Handle: "as13335", Entities: []rdapEntity{{ID: "AUTNUM13335"}}},
		{Handle: "MAINT-EXAMPLE"},
		{Handle: "AS not a number"},
	}}
	got := collectRDAPASNs(&v)
	if len(got) != 1 {
		t.Fatalf("asns=%v, want exactly {13335}", got)
	}
	if _, ok := got[13335]; !ok {
		t.Fatalf("asns=%v", got)
	}
}

func TestVerifyPrefixBelongsToASN(t *testing.T) {
	p := netip.MustParsePrefix("104.16.0.0/20")
	ctx := context.Background()

	okRes := NewResolver("", &fakeFetcher{byIP: map[string]string{
		"104.16.0.0": rdapWithASN(13335),
	}}, nil, 0)
	if ok, err := okRes.VerifyPrefixBelongsToASN(ctx, p, 13335); !ok || err != nil {
		t.Fatalf("match: ok=%v err=%v", ok, err)
	}

	badRes := NewResolver("", &fakeFetcher{byIP: map[string]string{
		"104.16.0.0": rdapWithASN(99999),
	}}, nil, 0)
	if ok, err := badRes.VerifyPrefixBelongsToASN(ctx, p, 13335); ok || err != nil {
		t.Fatalf("mismatch must reject without error: ok=%v err=%v", ok, err)
	}

	downRes := NewResolver("", &fakeFetcher{err: fmt.Errorf("no route")}, nil, 0)
	if ok, err := downRes.VerifyPrefixBelongsToASN(ctx, p, 13335); !ok || err == nil {
		t.Fatalf("rdap down must be fail-open: ok=%v err=%v", ok, err)
	}

	emptyRes := NewResolver("", &fakeFetcher{}, nil, 0)
	if ok, err := emptyRes.VerifyPrefixBelongsToASN(ctx, p, 13335); !ok || err != nil {
		t.Fatalf("no entities: ok=%v err=%v", ok, err)
	}
}

// TestDNSCacheEvictsExpired — при переполнении просроченные A-записи
// вычищаются, карта не растёт неограниченно при обходе множества доменов.
func TestDNSCacheEvictsExpired(t *testing.T) {
	r := NewResolver("", nil, nil, 0)

	r.dnsMu.Lock()
	r.dnsCache = map[string]dnsEntry{}
	for i := 0; i < dnsCacheMaxEntries; i++ {
		r.dnsCache["expired-"+strconv.Itoa(i)] = dnsEntry{ips: []string{"1.1.1.1"}, exp: time.Now().Add(-time.Minute)}
	}
	r.dnsCache["live"] = dnsEntry{ips: []string{"2.2.2.2"}, exp: time.Now().Add(time.Hour)}
	r.dnsMu.Unlock()

	r.dnsPut("fresh", []string{"3.3.3.3"})

	r.dnsMu.Lock()
	size := len(r.dnsCache)
	_, liveKept := r.dnsCache["live"]
	_, freshKept := r.dnsCache["fresh"]
	r.dnsMu.Unlock()

	if !liveKept || !freshKept {
		t.Fatalf("live/fresh must survive eviction (live=%v fresh=%v)", liveKept, freshKept)
	}
	if size > dnsCacheMaxEntries {
		t.Fatalf("cache not evicted: size=%d", size)
	}
}

// TestDNSGetDeletesExpired — просроченная запись удаляется при чтении.
func TestDNSGetDeletesExpired(t *testing.T) {
	r := NewResolver("", nil, nil, 0)
	r.dnsMu.Lock()
	r.dnsCache = map[string]dnsEntry{"x.net": {ips: []string{"1.1.1.1"}, exp: time.Now().Add(-time.Second)}}
	r.dnsMu.Unlock()

	if _, ok := r.dnsGet("x.net"); ok {
		t.Fatal("expired entry must miss")
	}
	r.dnsMu.Lock()
	_, still := r.dnsCache["x.net"]
	r.dnsMu.Unlock()
	if still {
		t.Fatal("expired entry must be deleted on read")
	}
}
