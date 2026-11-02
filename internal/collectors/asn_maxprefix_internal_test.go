package collectors

import (
	"context"
	"strings"
	"testing"
)

// TestASNCollectorMaxPrefixes — кап применяется к методу asn, но только
// положительный (0 = без капа: большие официальные ASN не режем).
func TestASNCollectorMaxPrefixes(t *testing.T) {
	body := `1.1.1.0/24 AS13335
1.1.2.0/24 AS13335
1.1.3.0/24 AS13335
1.1.4.0/24 AS13335
9.9.9.0/24 AS64500`
	h := fastHTTP(t, &stubTransport{status: 200, body: body})

	noCap := NewASNCollector(13335, h, nil, 0)
	res, err := noCap.Collect(context.Background(), "svc", Options{})
	if err != nil {
		t.Fatalf("collect: %v", err)
	}
	if len(res.Prefixes) != 4 {
		t.Fatalf("no-cap len=%d, want 4", len(res.Prefixes))
	}

	capped := NewASNCollector(13335, h, nil, 0).WithMaxPrefixes(2)
	res, err = capped.Collect(context.Background(), "svc", Options{})
	if err != nil {
		t.Fatalf("collect capped: %v", err)
	}
	if len(res.Prefixes) != 2 {
		t.Fatalf("capped len=%d, want 2", len(res.Prefixes))
	}
	// Проверка, что реально отдаёт AS13335, а не что попало.
	for _, p := range res.Prefixes {
		if !strings.HasPrefix(p.Addr().String(), "1.1.") {
			t.Fatalf("unexpected prefix %v", p)
		}
	}
}
