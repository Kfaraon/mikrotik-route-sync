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
