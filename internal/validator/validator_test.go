package validator

import (
	"net/netip"
	"strings"
	"testing"

	"github.com/Kfaraon/mikrotik-route-sync/internal/aggregator"
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

// Пример PROMPT II.3: include_only 1.2.3.0/24 + exclude 1.2.3.4/32 —
// хост вычитается из /24, остальные адреса сохраняются (раньше весь /24
// отбрасывался целиком и синхронизация падала по fail-closed).
func TestValidateExcludeHostSplitsContainingPrefix(t *testing.T) {
	v := Validator{Safety: defaultSafety()}
	ov := config.ServiceOverride{
		IncludeOnly: []string{"1.2.3.0/24"},
		Exclude:     []string{"1.2.3.4/32"},
	}
	out, err := v.Validate([]string{"1.2.3.0/24", "1.2.4.0/24"}, ov)
	if err != nil {
		t.Fatalf("validate: %v", err)
	}
	// 1.2.4.0/24 вне include_only отброшен.
	if got := aggregator.SumAddresses(out); got != 255 {
		t.Fatalf("expected 255 addresses (256-1), got %d", got)
	}
	excluded := "1.2.3.4"
	for _, p := range out {
		if p.Contains(mustAddr(t, excluded)) {
			t.Fatalf("excluded host covered by %s", p)
		}
		if !netipMustPrefix(t, "1.2.3.0/24").Contains(p.Addr()) {
			t.Fatalf("piece %s escapes include_only", p)
		}
	}
}

// Исключение, равное входному префиксу: всё отброшено → fail-closed ошибка.
func TestValidateExcludeEqualPrefixFailsClosed(t *testing.T) {
	v := Validator{Safety: defaultSafety()}
	ov := config.ServiceOverride{Exclude: []string{"8.8.8.0/24"}}
	if _, err := v.Validate([]string{"8.8.8.0/24"}, ov); err == nil {
		t.Fatal("full exclusion must fail-closed")
	}
}

// Исключение, шире входного префикса: уже ничего не осталось → fail-closed.
func TestValidateExcludeBroaderRemovesNarrower(t *testing.T) {
	v := Validator{Safety: defaultSafety()}
	ov := config.ServiceOverride{Exclude: []string{"8.8.0.0/16"}}
	if _, err := v.Validate([]string{"8.8.8.0/24"}, ov); err == nil {
		t.Fatal("prefix inside broader exclude must fail-closed")
	}
	out, err := v.Validate([]string{"8.8.8.0/24", "1.1.1.0/24"}, ov)
	if err != nil {
		t.Fatalf("validate: %v", err)
	}
	if len(out) != 1 || out[0].String() != "1.1.1.0/24" {
		t.Fatalf("expected only 1.1.1.0/24, got %v", out)
	}
}

// Fail-closed: опечатка в exclude/include не игнорируется молча.
func TestValidateInvalidOverrideFiltersFailClosed(t *testing.T) {
	v := Validator{Safety: defaultSafety()}

	_, err := v.Validate([]string{"8.8.8.0/24"},
		config.ServiceOverride{Exclude: []string{"not-a-cidr"}})
	if err == nil || !strings.Contains(err.Error(), "overrides.exclude") {
		t.Fatalf("invalid exclude must fail-closed, got %v", err)
	}

	_, err = v.Validate([]string{"8.8.8.0/24"},
		config.ServiceOverride{IncludeOnly: []string{"1.2.3"}})
	if err == nil || !strings.Contains(err.Error(), "overrides.include_only") {
		t.Fatalf("invalid include_only must fail-closed, got %v", err)
	}
}

func mustAddr(t *testing.T, s string) netip.Addr {
	t.Helper()
	a, err := netip.ParseAddr(s)
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func netipMustPrefix(t *testing.T, s string) netip.Prefix {
	t.Helper()
	p, err := netip.ParsePrefix(s)
	if err != nil {
		t.Fatal(err)
	}
	return p.Masked()
}
