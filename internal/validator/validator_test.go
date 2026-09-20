package validator

import (
	"testing"

	"github.com/Kfaraon/mikrotik-route-sync/internal/config"
)

func defaultSafety() config.SafetyConfig {
	return config.SafetyConfig{MinPrefixV4: 8}
}

func TestValidateFiltersPrivateAndHost(t *testing.T) {
	v := Validator{Safety: defaultSafety()}
	out, err := v.Validate([]string{
		"8.8.8.0/24",     // РѕРє
		"192.168.1.0/24", // private
		"100.64.0.0/10",  // cgnat
		"8.8.9.0/32",     // host route (allow_host_routes=false)
	}, config.ServiceOverride{})
	if err != nil {
		t.Fatalf("validate: %v", err)
	}
	if len(out) != 1 || out[0].String() != "8.8.8.0/24" {
		t.Fatalf("expected only 8.8.8.0/24, got %v", out)
	}
}

func TestValidateAllowsHostRoutesWhenEnabled(t *testing.T) {
	safety := defaultSafety()
	safety.AllowHostRoutes = true
	v := Validator{Safety: safety}
	out, err := v.Validate([]string{"8.8.8.8/32"}, config.ServiceOverride{})
	if err != nil {
		t.Fatalf("validate: %v", err)
	}
	if len(out) != 1 {
		t.Fatalf("expected host route kept, got %v", out)
	}
}

func TestValidateExcludeAndIncludeOnly(t *testing.T) {
	v := Validator{Safety: defaultSafety()}
	ov := config.ServiceOverride{
		IncludeOnly: []string{"8.8.8.0/24", "8.8.9.0/24"},
		Exclude:     []string{"8.8.9.0/24"},
	}
	out, err := v.Validate([]string{"8.8.8.0/24", "8.8.9.0/24", "8.8.10.0/24"}, ov)
	if err != nil {
		t.Fatalf("validate: %v", err)
	}
	if len(out) != 1 || out[0].String() != "8.8.8.0/24" {
		t.Fatalf("expected only 8.8.8.0/24, got %v", out)
	}
}

func TestValidateFailsClosedOnEmpty(t *testing.T) {
	v := Validator{Safety: defaultSafety()}
	if _, err := v.Validate(nil, config.ServiceOverride{}); err == nil {
		t.Fatal("expected error for empty input")
	}
	if _, err := v.Validate([]string{"10.0.0.0/8"}, config.ServiceOverride{}); err == nil {
		t.Fatal("expected error when everything filtered out")
	}
}

func TestValidateRejectsIPv6(t *testing.T) {
	v := Validator{Safety: defaultSafety()}
	out, err := v.Validate([]string{"2001:db8::/32", "2606:4700::/32"}, config.ServiceOverride{})
	if err == nil {
		t.Fatalf("IPv6-only input must fail-closed, got %v", out)
	}

	out, err = v.Validate([]string{"8.8.8.0/24", "2606:4700::/32"}, config.ServiceOverride{})
	if err != nil {
		t.Fatalf("mixed input must pass: %v", err)
	}
	if len(out) != 1 || out[0].String() != "8.8.8.0/24" {
		t.Fatalf("IPv6 must be dropped, got %v", out)
	}
}

func TestSanitizeComment(t *testing.T) {
	if err := SanitizeComment("instagram"); err != nil {
		t.Fatalf("plain name must pass: %v", err)
	}
	for _, bad := range []string{`with"quote`, `back\slash`, "new\nline"} {
		if err := SanitizeComment(bad); err == nil {
			t.Fatalf("expected rejection of %q", bad)
		}
	}
}
