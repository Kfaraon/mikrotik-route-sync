package core

// Тесты RDAP-проверки принадлежности (syncer.go verifyASNOwnership):
// малые наборы проверяются целиком — на фейковом RDAP, без сети.

import (
	"context"
	"fmt"
	"net/netip"
	"strings"
	"testing"

	"github.com/Kfaraon/mikrotik-list-sync/internal/collectors"
	"github.com/Kfaraon/mikrotik-list-sync/internal/config"
	"github.com/Kfaraon/mikrotik-list-sync/internal/resolver"
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

// TestCollectPresetASNKeepsHostsIntact: при составном методе "preset,asn"
// RDAP-проверка ASN не должна затрагивать точные /32 доменов пресета —
// их ASN может отличаться (CDN/хостинг/бренд-сквоттеры). Проверяется только
// asn-вклад.
func TestCollectPresetASNKeepsHostsIntact(t *testing.T) {
	s, _ := newTestSyncer(t, noopRouter)
	// RDAP отвергает всё, кроме 10.0.x — чтобы проверить, что /32 не проверяются.
	s.resPtr.Store(resolver.NewResolver("", &rdapFakeFetch{bad: map[string]bool{"157.240.5.5": true, "157.240.6.6": true}}, nil, 0))

	reg := collectors.NewRegistry(s.deps.Load())
	reg.Register("preset", func(p *collectors.Params) (collectors.Collector, error) {
		return &fakeCollector{src: &fakeSource{prefixes: []string{"157.240.5.5/32", "157.240.6.6/32"}}}, nil
	})
	reg.Register("asn", func(p *collectors.Params) (collectors.Collector, error) {
		return &fakeCollector{src: &fakeSource{prefixes: []string{"157.240.0.0/16", "157.241.0.0/16"}}}, nil
	})
	s.regPtr.Store(reg)

	ov := config.ServiceOverride{Method: "preset,asn", ASN: 32934, Preset: "", Domains: []string{"instagram.com"}}
	// Вручную прогоняем effectiveMethod: без пресета он вернёт ov.Method как есть.
	prefixes, _, err := s.collect(context.Background(), "instagram.com", ov, 0, nil)
	if err != nil {
		t.Fatalf("collect: %v", err)
	}
	// /32 должны остаться, широкие сети ASN — тоже (все «свои» для RDAP-стаба).
	hosts := 0
	for _, p := range prefixes {
		if p.Bits() == 32 {
			hosts++
		}
	}
	if hosts != 2 {
		t.Fatalf("preset /32 not preserved: %v", prefixes)
	}
}

// TestCollectExplicitASNBeatsResolver: явный ASN из override имеет приоритет
// над ASN классификатора и рантайм-резолвом. Без этого Meta-семейство
// (instagram/facebook/whatsapp) зависело бы от DNS-ответа витринного домена,
// а общий для них AS32934 подставлялся бы неверно.
func TestCollectExplicitASNBeatsResolver(t *testing.T) {
	s, _ := newTestSyncer(t, noopRouter)
	// DNS в никуда: если бы код попытался резолвить ASN, тест это заметил бы
	// по нулю в Params.ASN.
	s.resPtr.Store(deadEndResolver())

	var gotASN int
	reg := collectors.NewRegistry(s.deps.Load())
	for _, m := range []string{"asn", "dynamic"} {
		method := m
		reg.Register(method, func(p *collectors.Params) (collectors.Collector, error) {
			gotASN = p.ASN
			return &fakeCollector{src: &fakeSource{
				prefixes: []string{"157.240.0.0/16"},
			}}, nil
		})
	}
	s.regPtr.Store(reg)

	ov := config.ServiceOverride{ASN: 32934}
	prefixes, used, err := s.collect(context.Background(), "instagram", ov, 100, nil)
	if err != nil {
		t.Fatalf("collect: %v", err)
	}
	if gotASN != 32934 {
		t.Fatalf("collector got ASN=%d, want explicit 32934", gotASN)
	}
	if len(prefixes) == 0 {
		t.Fatalf("no prefixes collected, used=%q", used)
	}
}
