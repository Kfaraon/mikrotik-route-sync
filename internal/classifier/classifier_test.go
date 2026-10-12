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

// TestClassifyNoHardcodedASNForDomainServices фиксирует инвариант: ASN не вшит
// в classifier. У сервисов с доменами он резолвится из домена в рантайме
// (core/collect.go → resolver.ResolveASN); у CDN-сервисов без домена —
// берётся из каталога config.cdn_sources по имени сервиса
// (core/collect.go → collectors.CDNSourceByName). Единственное исключение —
// akamai: ASN задан вручную, т.к. это вход метода asn (официального списка нет).
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

	// CDN-сервисы без доменов: ASN тоже не вшит — источник в конфиге.
	for _, name := range []string{"cloudflare", "aws", "google", "fastly"} {
		got := Classify(name, "")
		if got.ASN != "" {
			t.Errorf("Classify(%q).ASN = %q, ожидалось пусто (ASN берётся из config.cdn_sources)", name, got.ASN)
		}
		if !contains(got.Methods, "cdn") {
			t.Errorf("Classify(%q).Methods = %v, ожидался метод cdn", name, got.Methods)
		}
	}

	// Исключение: akamai — метод asn, ASN задан (официального списка нет).
	if got := Classify("akamai", ""); got.ASN != "AS20940" {
		t.Errorf("Classify(\"akamai\").ASN = %q, ожидалось AS20940", got.ASN)
	}
}

func contains(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}

// Сервис, названный своим доменом (youtube.com), должен опознаваться как
// известный сервис youtube, а не уходить в широкий whois.
func TestClassifyServiceByNameDomain(t *testing.T) {
	cases := []struct {
		service string
		methods []string
	}{
		{"youtube.com", []string{"dynamic", "cdn"}},
		{"instagram.com", []string{"dynamic", "asn"}},
		{"facebook.com", []string{"dynamic", "asn"}},
		{"telegram.org", []string{"dynamic", "asn", "static_url"}},
		// Домен, которого нет в словаре, остаётся whois.
		{"rutor.org", []string{"whois"}},
	}
	for _, c := range cases {
		got := Classify(c.service, "")
		if !reflect.DeepEqual(got.Methods, c.methods) {
			t.Errorf("Classify(%q).Methods = %v, want %v", c.service, got.Methods, c.methods)
		}
	}
	// Домены записи подставляются: collect() резолвит по ним ASN.
	if got := Classify("youtube.com", ""); len(got.Domains) == 0 || got.Domains[0] != "youtube.com" {
		t.Fatalf("Classify(\"youtube.com\").Domains = %v", got.Domains)
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

// H9: override-метод не должен терять распознавание IP/ASN-литерала —
// иначе `8.8.8.8` + method: whois уходил в whois с пустым вводом.
func TestClassifyOverrideKeepsLiterals(t *testing.T) {
	ip := Classify("8.8.8.8", "whois")
	if !reflect.DeepEqual(ip.IPs, []string{"8.8.8.8"}) || !reflect.DeepEqual(ip.Methods, []string{"whois"}) {
		t.Fatalf("override IP literal lost: %+v", ip)
	}
	asn := Classify("as13335", "cdn")
	if asn.ASN != "AS13335" {
		t.Fatalf("override ASN literal lost: %+v", asn)
	}
}
