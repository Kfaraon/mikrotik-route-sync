package classifier

import (
	"reflect"
	"testing"
)

func TestClassifyKnownServices(t *testing.T) {
	cases := []struct {
		service string
		methods []string
	}{
		{"cloudflare", []string{"cdn"}},
		{"youtube", []string{"dynamic", "cdn"}},
		{"instagram", []string{"dynamic", "asn"}},
		{"telegram", []string{"dynamic", "asn", "static_url"}},
		{"fastly", []string{"cdn"}},
		{"akamai", []string{"asn"}},
		{"AS13335", []string{"asn"}},
		{"rutor.org", []string{"whois"}},
		{"8.8.8.8", []string{"whois"}},
	}
	for _, c := range cases {
		got := Classify(c.service, "")
		if !reflect.DeepEqual(got.Methods, c.methods) {
			t.Errorf("Classify(%q).Methods = %v, want %v", c.service, got.Methods, c.methods)
		}
	}
}

func TestClassifyOverrideMultiMethod(t *testing.T) {
	got := Classify("myservice", "cdn,dynamic")
	if !reflect.DeepEqual(got.Methods, []string{"cdn", "dynamic"}) {
		t.Fatalf("expected [cdn dynamic], got %v", got.Methods)
	}
}

// TestClassifyNoHardcodedASNForDomainServices фиксирует инвариант: у сервисов
// с доменами ASN не вшит (определяется резолвом в рантайме через
// core/collect.go → resolver.ResolveASN), а задан только там, где ASN —
// идентификатор источника (CDN-список без домена, akamai → метод asn).
// Регрессия с зашитым AS62240 у telegram вела на чужие сети Clouvider.
func TestClassifyNoHardcodedASNForDomainServices(t *testing.T) {
	// Доменные сервисы: ASN обязан быть пустым, домены — присутствовать.
	for _, name := range []string{"youtube", "instagram", "facebook", "telegram"} {
		got := Classify(name, "")
		if got.ASN != "" {
			t.Errorf("Classify(%q).ASN = %q, ожидалось пусто (ASN определяется рантаймом)", name, got.ASN)
		}
		if len(got.Domains) == 0 {
			t.Errorf("Classify(%q).Domains пуст — рантайм-резолв ASN не сработает", name)
		}
	}

	// Источниковые сервисы: ASN задан (ключ cdnSources / метод asn).
	src := map[string]string{
		"cloudflare": "AS13335",
		"aws":        "AS16509",
		"google":     "AS15169",
		"fastly":     "AS54113",
		"akamai":     "AS20940",
	}
	for name, want := range src {
		if got := Classify(name, ""); got.ASN != want {
			t.Errorf("Classify(%q).ASN = %q, ожидалось %q", name, got.ASN, want)
		}
	}
}

func TestClassifyASNLiteral(t *testing.T) {
	got := Classify("as15169", "")
	if got.ASN != "AS15169" || !reflect.DeepEqual(got.Methods, []string{"asn"}) {
		t.Fatalf("unexpected: %+v", got)
	}
}

func TestClassifyIPLiteral(t *testing.T) {
	got := Classify("9.9.9.9", "")
	if !reflect.DeepEqual(got.IPs, []string{"9.9.9.9"}) || !reflect.DeepEqual(got.Methods, []string{"whois"}) {
		t.Fatalf("unexpected: %+v", got)
	}
}

func TestClassifyTelegramStaticURL(t *testing.T) {
	got := Classify("telegram", "")
	if len(got.StaticURLs) != 1 || got.StaticURLs[0] != "https://core.telegram.org/resources/cidr.txt" {
		t.Fatalf("telegram static=%v", got.StaticURLs)
	}
	// Явный метод static_url подхватывает курированный URL.
	ov := Classify("telegram", "static_url")
	if len(ov.StaticURLs) != 1 || ov.StaticURLs[0] != got.StaticURLs[0] {
		t.Fatalf("override static_url lost curated URL: %+v", ov)
	}
}

func TestClassifyOverridePlusSeparated(t *testing.T) {
	got := Classify("myservice", "dynamic+cdn")
	if !reflect.DeepEqual(got.Methods, []string{"dynamic", "cdn"}) {
		t.Fatalf("expected [dynamic cdn], got %v", got.Methods)
	}
}
