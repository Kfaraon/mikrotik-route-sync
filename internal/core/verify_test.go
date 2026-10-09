package core

// Тесты RDAP-проверки принадлежности (syncer.go verifyASNOwnership):
// малые наборы проверяются целиком — на фейковом RDAP, без сети.

import (
	"context"
	"fmt"
	"net/netip"
	"strings"
	"testing"

	"github.com/Kfaraon/mikrotik-route-sync/internal/resolver"
)

func TestRdapVerifyStep(t *testing.T) {
	cases := []struct {
		n, want int
	}{
		{0, 1}, {1, 1}, {32, 1}, {100, 1},
		{101, 1}, {1000, 10},
	}
	for _, c := range cases {
		if got := rdapVerifyStep(c.n); got != c.want {
			t.Errorf("rdapVerifyStep(%d)=%d, want %d", c.n, got, c.want)
		}
	}
}

// rdapFakeFetch — RDAP-заглушка: bad-IP отдаёт чужой ASN.
type rdapFakeFetch struct {
	bad map[string]bool
}

func (f *rdapFakeFetch) Fetch(_ context.Context, url string) ([]byte, error) {
	ip := url[strings.LastIndex(url, "/")+1:]
	asn := 13335
	if f.bad[ip] {
		asn = 99999
	}
	return []byte(fmt.Sprintf(`{"entities":[{"handle":"AS%d"}]}`, asn)), nil
}

func TestVerifyASNOwnershipChecksSmallSetsFully(t *testing.T) {
	s, _ := newTestSyncer(t, noopRouter)
	bad := map[string]bool{"10.0.1.0": true, "10.0.3.0": true}
	s.resPtr.Store(resolver.NewResolver("", &rdapFakeFetch{bad: bad}, nil, 0))

	var in []netip.Prefix
	for i := 0; i < 70; i++ {
		in = append(in, netip.MustParsePrefix(fmt.Sprintf("10.0.%d.0/24", i)))
	}

	out := s.verifyASNOwnership(context.Background(), in, 13335)
	if len(out) != 68 {
		t.Fatalf("len=%d, want 68 (2 чужих отклонены)", len(out))
	}
	for _, p := range out {
		if bad[p.Addr().String()] {
			t.Fatalf("чужой префикс не отклонён: %v", p)
		}
	}
}

// H4: глобальная RDAP-проверка по одному ASN не должна применяться к whois:
// whois собирает префиксы всех ASN, найденных по IP домена, и проверка против
// основного ASN отвергала бы легитимные вторичные префиксы.
func TestShouldVerifyASNOwnership(t *testing.T) {
	cases := []struct {
		used string
		asn  int
		want bool
	}{
		{"asn", 13335, true},
		{"dynamic+asn", 13335, true},
		{"asn,dynamic", 13335, true},
		{"whois", 13335, false},
		{"dynamic+whois", 13335, false},
		{"asn+whois", 13335, false},
		{"dynamic", 13335, false},
		{"asn", 0, false},
	}
	for _, c := range cases {
		if got := shouldVerifyASNOwnership(c.used, c.asn); got != c.want {
			t.Errorf("shouldVerifyASNOwnership(%q, %d)=%v, want %v", c.used, c.asn, got, c.want)
		}
	}
}

func TestVerifyASNOwnershipAllGoodKeepsAll(t *testing.T) {
	s, _ := newTestSyncer(t, noopRouter)
	s.resPtr.Store(resolver.NewResolver("", &rdapFakeFetch{}, nil, 0))

	in := []netip.Prefix{
		netip.MustParsePrefix("104.16.0.0/20"),
		netip.MustParsePrefix("172.64.0.0/16"),
	}
	if out := s.verifyASNOwnership(context.Background(), in, 13335); len(out) != 2 {
		t.Fatalf("len=%d, want 2", len(out))
	}
}
