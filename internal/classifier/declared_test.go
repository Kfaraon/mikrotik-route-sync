package classifier

import "testing"

// TestDeclaredASN — объявленные ASN Meta-семейства (instagram/facebook)
// доступны по имени и домену; для сервисов без объявления — false.
func TestDeclaredASN(t *testing.T) {
	cases := []struct {
		service string
		want    int
		ok      bool
	}{
		{"instagram", 32934, true},
		{"instagram.com", 32934, true},
		{"facebook", 32934, true},
		{"facebook.com", 32934, true},
		{"youtube", 0, false},
		{"cloudflare", 0, false},
		{"akamai", 0, false}, // объявлен через asn (строка), не declaredASN
		{"unknown.example", 0, false},
	}
	for _, c := range cases {
		got, ok := DeclaredASN(c.service)
		if ok != c.ok || got != c.want {
			t.Errorf("DeclaredASN(%q)=(%d,%v), want (%d,%v)", c.service, got, ok, c.want, c.ok)
		}
	}
}
