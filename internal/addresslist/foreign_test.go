package addresslist

import (
	"net/netip"
	"testing"
)

func mustPref(t *testing.T, s string) netip.Prefix {
	t.Helper()
	p, err := netip.ParsePrefix(s)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func mustAddr(t *testing.T, s string) netip.Addr {
	t.Helper()
	a, err := netip.ParseAddr(s)
	if err != nil {
		t.Fatal(err)
	}
	return a
}

// sumOut суммирует число адресов в наборе CIDR-строк.
func sumOut(t *testing.T, in []string) int {
	t.Helper()
	total := 0
	for _, s := range in {
		p := mustPref(t, s)
		total += 1 << uint(32-p.Bits())
	}
	return total
}

func TestForeignEntriesPartitions(t *testing.T) {
	entries := []Entry{
		{ID: "*1", Address: "8.8.8.0/24", List: "TO-VPN", Comment: "AUTO:demo"},
		{ID: "*2", Address: "9.9.9.0/24", List: "TO-VPN", Comment: "AUTO:demo", Dynamic: "true"},
		{ID: "*3", Address: "1.1.1.0/24", List: "TO-VPN", Comment: "AUTO:other"},
		{ID: "*4", Address: "5.5.5.0/24", List: "TO-VPN", Comment: "Cloudflare"},
		{ID: "*5", Address: "4.4.4.0/24", List: "TO-VPN"},
		{ID: "*6", Address: "6.6.6.0/24", List: "OTHER", Comment: "AUTO:demo"},
	}

	own := FilterManaged(entries, "TO-VPN", "AUTO:demo")
	if len(own) != 1 || own[0].ID != "*1" {
		t.Fatalf("own managed: %v", own)
	}

	foreign := ForeignEntries(entries, "TO-VPN", "AUTO:demo")
	// *1 — наша; *6 — чужой список (не учитывается вовсе);
	// *2 (dynamic с нашим комментарием), *3, *4, *5 — чужие.
	if len(foreign) != 4 {
		t.Fatalf("expected 4 foreign entries, got %d: %v", len(foreign), foreign)
	}
	ids := map[string]bool{}
	for _, e := range foreign {
		ids[e.ID] = true
	}
	for _, want := range []string{"*2", "*3", "*4", "*5"} {
		if !ids[want] {
			t.Fatalf("foreign must contain %s: %v", want, ids)
		}
	}
}

func TestSubtractForeignExactDuplicate(t *testing.T) {
	out, hits := SubtractForeign(
		[]string{"5.5.5.0/24", "8.8.8.0/24"},
		[]Entry{{Address: "5.5.5.0/24", List: "TO-VPN", Comment: "Manual"}},
	)
	if hits != 1 {
		t.Fatalf("expected 1 hit, got %d", hits)
	}
	if len(out) != 1 || out[0] != "8.8.8.0/24" {
		t.Fatalf("exact duplicate must vanish: %v", out)
	}
}

func TestSubtractForeignSplitsContainingPrefix(t *testing.T) {
	// Чужой /25 внутри нашего /24 → остаётся вторая половина.
	out, hits := SubtractForeign(
		[]string{"9.9.9.0/24"},
		[]Entry{{Address: "9.9.9.0/25", List: "TO-VPN", Comment: "Manual"}},
	)
	if hits != 1 || len(out) != 1 || out[0] != "9.9.9.128/25" {
		t.Fatalf("expected [9.9.9.128/25], got hits=%d out=%v", hits, out)
	}
}

func TestSubtractForeignContainedByDesired(t *testing.T) {
	// Чужой хост внутри нашего префикса → вычитание без пересечения.
	out, hits := SubtractForeign(
		[]string{"8.8.8.0/24"},
		[]Entry{{Address: "8.8.8.8", List: "TO-VPN", Comment: "Manual"}},
	)
	if hits != 1 {
		t.Fatalf("expected 1 hit, got %d", hits)
	}
	if len(out) == 0 {
		t.Fatal("expected remainder pieces")
	}
	// Сумма адресов: 256 - 1 = 255.
	if got := sumOut(t, out); got != 255 {
		t.Fatalf("expected 255 addresses, got %d", got)
	}
	for _, d := range out {
		if p := mustPref(t, d); p.Contains(mustAddr(t, "8.8.8.8")) {
			t.Fatalf("excluded host covered by %s", d)
		}
	}
}

func TestSubtractForeignDisjointAndInvalid(t *testing.T) {
	// Разрыв CIDR-блоков: без изменений и без пересечений.
	out, hits := SubtractForeign(
		[]string{"8.8.8.0/24"},
		[]Entry{{Address: "1.1.1.0/24", List: "TO-VPN", Comment: "Manual"}},
	)
	if hits != 0 || len(out) != 1 || out[0] != "8.8.8.0/24" {
		t.Fatalf("disjoint must be unchanged: hits=%d out=%v", hits, out)
	}

	// Некорректный (hostname) и IPv6 чужие записи пропускаются.
	out, hits = SubtractForeign(
		[]string{"8.8.8.0/24"},
		[]Entry{
			{Address: "download.proxmox.com", List: "TO-VPN", Comment: "Manual"},
			{Address: "2001:db8::/32", List: "TO-VPN", Comment: "Manual"},
		},
	)
	if hits != 0 || len(out) != 1 || out[0] != "8.8.8.0/24" {
		t.Fatalf("unparseable foreign must be skipped: hits=%d out=%v", hits, out)
	}

	// Пустые аргументы — без изменений.
	if out, hits := SubtractForeign(nil, []Entry{{Address: "8.8.8.0/24"}}); out != nil || hits != 0 {
		t.Fatalf("empty desired: %v %d", out, hits)
	}
	if out, hits := SubtractForeign([]string{"8.8.8.0/24"}, nil); hits != 0 || len(out) != 1 {
		t.Fatalf("empty foreign: %v %d", out, hits)
	}
}

// Повторное вычитание идемпотентно: применение к уже вычтенному набору ничего
// не меняет (пересечений больше нет).
func TestSubtractForeignIdempotent(t *testing.T) {
	foreign := []Entry{
		{Address: "9.9.9.0/25", List: "TO-VPN", Comment: "Manual"},
	}
	once, hits := SubtractForeign([]string{"9.9.9.0/24"}, foreign)
	if hits != 1 || len(once) != 1 {
		t.Fatalf("first pass: hits=%d out=%v", hits, once)
	}
	twice, hits2 := SubtractForeign(once, foreign)
	if hits2 != 0 || len(twice) != 1 || twice[0] != once[0] {
		t.Fatalf("second pass must be a no-op: hits=%d out=%v", hits2, twice)
	}
}
