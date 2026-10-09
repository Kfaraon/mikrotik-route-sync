package config

import "testing"

func TestCDNSourcesDefaultsApplied(t *testing.T) {
	cfg, err := Load(writeTempConfig(t, minimalYAML))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(cfg.CDNSources) != 4 {
		t.Fatalf("want 4 built-in CDN sources, got %d: %+v", len(cfg.CDNSources), cfg.CDNSources)
	}
	bySvc := map[string]CDNSourceConfig{}
	for _, s := range cfg.CDNSources {
		bySvc[s.Service] = s
	}
	want := map[string]int{"cloudflare": 13335, "aws": 16509, "google": 15169, "fastly": 54113}
	for svc, asn := range want {
		got, ok := bySvc[svc]
		if !ok {
			t.Errorf("missing default source %q", svc)
			continue
		}
		if got.ASN != asn {
			t.Errorf("source %q ASN = %d, want %d", svc, got.ASN, asn)
		}
		if got.URL == "" || got.Parser == "" || got.Name == "" {
			t.Errorf("source %q incomplete: %+v", svc, got)
		}
	}
}

func TestCDNSourcesExplicitOverrideKept(t *testing.T) {
	cfg, err := Load(writeTempConfig(t, `
timezone: UTC
mikrotik: {host: h, username: u, password: p}
firewall: {address_list: TO-VPN}
schedules: {global: "every 6h"}
cdn_sources:
  - {service: custom, name: Custom, asn: 64500, url: "https://example.com/list.txt", parser: text}
`))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(cfg.CDNSources) != 1 || cfg.CDNSources[0].Service != "custom" {
		t.Fatalf("explicit cdn_sources must not be overwritten: %+v", cfg.CDNSources)
	}
}

func TestValidateCDNSourcesRejects(t *testing.T) {
	base := `
timezone: UTC
mikrotik: {host: h, username: u, password: p}
firewall: {address_list: TO-VPN}
schedules: {global: "every 6h"}
cdn_sources:
`
	cases := []struct {
		name string
		body string
	}{
		{"bad service", base + "  - {service: \"Bad Name\", name: X, asn: 1, url: \"https://e.com\", parser: text}\n"},
		{"empty name", base + "  - {service: ok, name: \"\", asn: 1, url: \"https://e.com\", parser: text}\n"},
		{"bad asn", base + "  - {service: ok, name: X, asn: 0, url: \"https://e.com\", parser: text}\n"},
		{"bad url", base + "  - {service: ok, name: X, asn: 1, url: \"ftp://e.com\", parser: text}\n"},
		{"bad parser", base + "  - {service: ok, name: X, asn: 1, url: \"https://e.com\", parser: bogus}\n"},
		{"dup service", base +
			"  - {service: ok, name: X, asn: 1, url: \"https://e.com\", parser: text}\n" +
			"  - {service: ok, name: Y, asn: 2, url: \"https://e2.com\", parser: text}\n"},
		{"dup asn", base +
			"  - {service: a, name: X, asn: 7, url: \"https://e.com\", parser: text}\n" +
			"  - {service: b, name: Y, asn: 7, url: \"https://e2.com\", parser: text}\n"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if _, err := Load(writeTempConfig(t, c.body)); err == nil {
				t.Fatal("expected validation error")
			}
		})
	}
}
