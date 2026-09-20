package scheduler

import (
	"testing"
	"time"
)

func TestToCron(t *testing.T) {
	cases := map[string]string{
		"0 */6 * * *":              "0 */6 * * *",
		"daily at 03:00":           "0 3 * * *",
		"weekly on sunday at 4:05": "5 4 * * 0",
	}
	for in, want := range cases {
		got, ok := toCron(in)
		if !ok || got != want {
			t.Errorf("toCron(%q) = %q,%v want %q", in, got, ok, want)
		}
	}
}

func TestInterval(t *testing.T) {
	d, ok := interval("every 6h")
	if !ok || d != 6*time.Hour {
		t.Fatalf("interval every 6h = %v,%v", d, ok)
	}
	if _, ok := interval("manual"); ok {
		t.Fatal("manual is not an interval")
	}
}

func TestValidSpec(t *testing.T) {
	for _, spec := range []string{"manual", "disabled", "inherit", "every 30m", "daily at 03:00", "0 3 * * *"} {
		if !ValidSpec(spec) {
			t.Errorf("ValidSpec(%q) = false", spec)
		}
	}
	for _, spec := range []string{"bogus", "every", "daily at 99:99", "bad spec here!!"} {
		if ValidSpec(spec) {
			t.Errorf("ValidSpec(%q) = true", spec)
		}
	}
}

func TestPurgeInterval(t *testing.T) {
	if d, ok := purgeInterval("every 1h"); !ok || d != time.Hour {
		t.Fatalf("purgeInterval = %v,%v", d, ok)
	}
	if _, ok := purgeInterval(""); ok {
		t.Fatal("empty spec must be not ok")
	}
}
