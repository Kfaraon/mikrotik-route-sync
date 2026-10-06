package core

// Тесты контроля покрытия A-записей (coverage.go): чистая checkCoverage
// на фейковом резолве, без сети.

import (
	"context"
	"net/netip"
	"testing"
)

func fakeResolve(domains map[string][]string, errOn map[string]bool) func(context.Context, string) ([]string, error) {
	return func(_ context.Context, d string) ([]string, error) {
		if errOn[d] {
			return nil, context.DeadlineExceeded
		}
		return domains[d], nil
	}
}

func mustPrefixes(ss ...string) []netip.Prefix {
	out := make([]netip.Prefix, 0, len(ss))
	for _, s := range ss {
		out = append(out, netip.MustParsePrefix(s))
	}
	return out
}

func TestCheckCoverageAllCovered(t *testing.T) {
	resolve := fakeResolve(map[string][]string{
		"x.net": {"104.16.1.1", "104.16.1.1"}, // дубль считается один раз
	}, nil)
	rep := checkCoverage(context.Background(), resolve, []string{"x.net"},
		mustPrefixes("104.16.0.0/16"))
	if !rep.Complete() || rep.Checked != 1 || rep.Covered != 1 {
		t.Fatalf("rep=%+v", rep)
	}
}

func TestCheckCoveragePartial(t *testing.T) {
	resolve := fakeResolve(map[string][]string{
		"a.net": {"10.1.0.5", "10.9.0.5"},
		"b.net": {"2a00::1", "10.1.0.5"}, // IPv6 пропускается, дубль не двоится
	}, nil)
	rep := checkCoverage(context.Background(), resolve, []string{"a.net", "b.net"},
		mustPrefixes("10.1.0.0/24"))
	if rep.Complete() {
		t.Fatal("partial coverage must not be complete")
	}
	if rep.Checked != 2 || rep.Covered != 1 {
		t.Fatalf("checked/covered wrong: %+v", rep)
	}
	if len(rep.Uncovered) != 1 || rep.Uncovered[0] != "10.9.0.5" {
		t.Fatalf("uncovered=%v", rep.Uncovered)
	}
}

func TestCheckCoverageResolveFailure(t *testing.T) {
	resolve := fakeResolve(nil, map[string]bool{"dead.net": true})
	rep := checkCoverage(context.Background(), resolve, []string{"dead.net"},
		mustPrefixes("10.0.0.0/8"))
	if !rep.Complete() || rep.Checked != 0 {
		t.Fatalf("resolve failure must give empty complete report: %+v", rep)
	}
}

func TestCheckCoverageNoDomains(t *testing.T) {
	called := false
	resolve := func(_ context.Context, _ string) ([]string, error) {
		called = true
		return []string{"1.1.1.1"}, nil
	}
	rep := checkCoverage(context.Background(), resolve, nil, mustPrefixes("0.0.0.0/0"))
	if called || !rep.Complete() {
		t.Fatalf("no domains: no resolve, complete report (called=%v, rep=%+v)", called, rep)
	}
}

func TestCoverageReportCompleteNil(t *testing.T) {
	var rep *CoverageReport
	if !rep.Complete() {
		t.Fatal("nil report must be complete")
	}
}
