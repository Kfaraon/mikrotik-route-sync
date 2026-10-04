package addresslist

import "testing"

func TestNormalizeAddress(t *testing.T) {
	cases := map[string]string{
		"8.8.8.0/24":     "8.8.8.0/24",
		"8.8.8.1/24":     "8.8.8.0/24", // хостовые биты обнуляются
		"8.8.8.8":        "8.8.8.8/32", // одиночный IP
		"172.20.10.0/28": "172.20.10.0/28",
	}
	for in, want := range cases {
		got, err := NormalizeAddress(in)
		if err != nil || got != want {
			t.Errorf("NormalizeAddress(%q) = %q,%v want %q", in, got, err, want)
		}
	}
	for _, bad := range []string{"", "2001:db8::/32", "host.example", "8.8.8.8/40"} {
		if _, err := NormalizeAddress(bad); err == nil {
			t.Errorf("expected error for %q", bad)
		}
	}
}

func TestCommentForAndServiceFromComment(t *testing.T) {
	if c := CommentFor("AUTO", "youtube"); c != "AUTO:youtube" {
		t.Fatalf("CommentFor: %q", c)
	}
	svc, ok := ServiceFromComment("AUTO:youtube", "AUTO")
	if !ok || svc != "youtube" {
		t.Fatalf("ServiceFromComment: %q %v", svc, ok)
	}
	if _, ok := ServiceFromComment("manual note", "AUTO"); ok {
		t.Fatal("non-AUTO comment accepted")
	}
	if _, ok := ServiceFromComment("AUTO:", "AUTO"); ok {
		t.Fatal("empty service accepted")
	}
}

func TestFilterManaged(t *testing.T) {
	in := []Entry{
		{ID: "*1", List: "TO-VPN", Address: "8.8.8.0/24", Comment: "AUTO:yt"},
		{ID: "*2", List: "OTHER", Address: "1.1.1.0/24", Comment: "AUTO:yt"},
		{ID: "*3", List: "TO-VPN", Address: "2.2.2.0/24", Comment: "AUTO:ig"},
		{ID: "*4", List: "TO-VPN", Address: "3.3.3.0/24", Comment: "AUTO:yt", Dynamic: "true"},
		{ID: "*5", List: "TO-VPN", Address: "4.4.4.0/24"},
	}
	got := FilterManaged(in, "TO-VPN", "AUTO:yt")
	if len(got) != 1 || got[0].ID != "*1" {
		t.Fatalf("FilterManaged wrong: %+v", got)
	}
}

func TestComputeDiffAndRemovalPlan(t *testing.T) {
	managed := []Entry{
		{ID: "*1", Address: "8.8.8.0/24", Comment: "AUTO:yt"},
		{ID: "*2", Address: "8.8.8.0/24", Comment: "AUTO:yt"}, // дубль
		{ID: "*3", Address: "1.1.1.0/24", Comment: "AUTO:yt"}, // устарел
	}
	desired := []string{"8.8.8.0/24", "9.9.9.0/24"}

	d, err := ComputeDiff("yt", "TO-VPN", "AUTO:yt", desired, managed)
	if err != nil {
		t.Fatal(err)
	}
	if len(d.Add) != 1 || d.Add[0] != "9.9.9.0/24" {
		t.Fatalf("add: %v", d.Add)
	}
	if len(d.Unchanged) != 1 || d.Unchanged[0] != "8.8.8.0/24" {
		t.Fatalf("unchanged: %v", d.Unchanged)
	}
	if len(d.Remove) != 2 {
		t.Fatalf("remove (дубль + устаревший): %v", d.Remove)
	}

	plan := RemovalPlan(desired, managed)
	// оставляем *1 (первый для 8.8.8.0), удаляем дубль *2 и устаревший *3
	if len(plan) != 2 || plan[0].ID != "*2" || plan[1].ID != "*3" {
		t.Fatalf("removal plan: %+v", plan)
	}
}

func TestCheckDeletion(t *testing.T) {
	p := SafetyParams{MaxDeleteRatio: 0.5, RequireConfirmationOver: 10}
	if err := CheckDeletion(100, 10, p); err != nil {
		t.Fatalf("10%% < 50%% must pass: %v", err)
	}
	if err := CheckDeletion(100, 60, p); err == nil {
		t.Fatal("60%% > 50%% must fail")
	}
	if err := CheckDeletion(0, 0, p); err != nil {
		t.Fatalf("no existing: no checks: %v", err)
	}
	if err := CheckDeletion(100, 11, SafetyParams{MaxDeleteRatio: 0.5, RequireConfirmationOver: 10}); err == nil {
		t.Fatal("absolute threshold must fail")
	}
}

// PROMPT II.6: если новый набор пуст (remove >= existing) — блок всегда,
// даже когда ratio-пороги это допускают.
func TestCheckDeletionTotalWipeBlocked(t *testing.T) {
	if err := CheckDeletion(5, 5, SafetyParams{MaxDeleteRatio: 1.0}); err == nil {
		t.Fatal("total wipe must be blocked even with MaxDeleteRatio=1.0")
	}
	if err := CheckDeletion(5, 5, SafetyParams{}); err == nil {
		t.Fatal("total wipe must be blocked with zero params")
	}
	if err := CheckDeletion(5, 4, SafetyParams{MaxDeleteRatio: 1.0}); err != nil {
		t.Fatalf("4 of 5 under ratio 1.0 must pass: %v", err)
	}
	if err := CheckDeletion(5, 0, SafetyParams{}); err != nil {
		t.Fatalf("no removals: no checks: %v", err)
	}
	if err := CheckDeletion(0, 0, SafetyParams{}); err != nil {
		t.Fatalf("no existing: no checks: %v", err)
	}
}

// PROMPT II.7: выключенная (disabled=true) управляемая запись не может быть
// unchanged — она уходит в update (re-enable).
func TestComputeDiffDisabledEntryGoesToUpdate(t *testing.T) {
	managed := []Entry{
		{ID: "*1", Address: "8.8.8.0/24", List: "TO-VPN", Comment: "AUTO:yt", Disabled: "true"},
		{ID: "*2", Address: "9.9.9.0/24", List: "TO-VPN", Comment: "AUTO:yt"},
	}
	desired := []string{"8.8.8.0/24"}

	d, err := ComputeDiff("yt", "TO-VPN", "AUTO:yt", desired, managed)
	if err != nil {
		t.Fatal(err)
	}
	if len(d.Update) != 1 || d.Update[0] != "8.8.8.0/24" {
		t.Fatalf("disabled entry must go to update, got %v", d.Update)
	}
	if len(d.Unchanged) != 0 {
		t.Fatalf("disabled entry must not be unchanged, got %v", d.Unchanged)
	}
	if len(d.Add) != 0 {
		t.Fatalf("add must be empty, got %v", d.Add)
	}
	if len(d.Remove) != 1 || d.Remove[0] != "9.9.9.0/24" {
		t.Fatalf("stale entry must be removed, got %v", d.Remove)
	}

	// UpdatePlan должен вернуть запись с .id для PATCH.
	up := UpdatePlan(desired, managed)
	if len(up) != 1 || up[0].ID != "*1" {
		t.Fatalf("UpdatePlan: %+v", up)
	}
}

// Из двух экземпляров одного адреса остаётся включённый: без PATCH, дубль
// (выключенный) удаляется.
func TestComputeDiffPrefersEnabledDuplicate(t *testing.T) {
	managed := []Entry{
		{ID: "*1", Address: "8.8.8.0/24", Disabled: "true"}, // выключенный идёт первым
		{ID: "*2", Address: "8.8.8.0/24"},                   // включённый дубль
	}
	desired := []string{"8.8.8.0/24"}

	d, err := ComputeDiff("yt", "TO-VPN", "AUTO:yt", desired, managed)
	if err != nil {
		t.Fatal(err)
	}
	if len(d.Unchanged) != 1 || d.Unchanged[0] != "8.8.8.0/24" {
		t.Fatalf("enabled duplicate must be kept as unchanged, got %v", d.Unchanged)
	}
	if len(d.Update) != 0 {
		t.Fatalf("no update needed when enabled duplicate exists, got %v", d.Update)
	}
	if len(d.Remove) != 1 || d.Remove[0] != "8.8.8.0/24" {
		t.Fatalf("disabled duplicate must be removed, got %v", d.Remove)
	}

	// RemovalPlan обязан удалить именно выключенный экземпляр *1.
	plan := RemovalPlan(desired, managed)
	if len(plan) != 1 || plan[0].ID != "*1" {
		t.Fatalf("removal plan must drop disabled duplicate *1: %+v", plan)
	}
}

// Идемпотентность (PROMPT II.7): повторный diff с теми же входными данными
// не порождает изменений.
func TestComputeDiffIdempotentWhenAllMatch(t *testing.T) {
	managed := []Entry{
		{ID: "*1", Address: "8.8.8.0/24"},
		{ID: "*2", Address: "1.1.1.0/24"},
	}
	desired := []string{"8.8.8.0/24", "1.1.1.0/24"}

	for i := 0; i < 2; i++ {
		d, err := ComputeDiff("yt", "TO-VPN", "AUTO:yt", desired, managed)
		if err != nil {
			t.Fatal(err)
		}
		if len(d.Add) != 0 || len(d.Remove) != 0 || len(d.Update) != 0 {
			t.Fatalf("run %d produced changes: %+v", i+1, d)
		}
		if len(d.Unchanged) != 2 {
			t.Fatalf("run %d unchanged: %v", i+1, d.Unchanged)
		}
	}
}
