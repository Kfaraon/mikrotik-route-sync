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
