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
		{"telegram", []string{"dynamic", "asn"}},
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
