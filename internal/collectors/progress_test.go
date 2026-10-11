package collectors

import (
	"context"
	"testing"
)

// report без привязанного прогресса не паникует и ничего не делает.
func TestReportWithoutProgressIsNoop(t *testing.T) {
	ctx := context.Background()
	Report(ctx, "шаг", 1, 10)
	ReportCreep(ctx, "шаг")
	ReportFraction(ctx, "шаг", 0.5)
}

// WithProgress с nil-функцией возвращает исходный контекст.
func TestWithProgressNilFuncKeepsContext(t *testing.T) {
	ctx := context.Background()
	if got := WithProgress(ctx, 10, 20, nil); got != ctx {
		t.Fatal("WithProgress(nil) must return the same context")
	}
}

// Процент внутри диапазона растёт по доле и не отпрыгивает назад.
func TestReportFractionMonotonic(t *testing.T) {
	var got []int
	ctx := WithProgress(context.Background(), 15, 40, func(p int, _ string) {
		got = append(got, p)
	})

	ReportFraction(ctx, "a", 0)
	if want := 15; got[len(got)-1] != want {
		t.Fatalf("start percent = %d, want %d", got[len(got)-1], want)
	}
	ReportFraction(ctx, "b", 0.5)
	if want := 28; got[len(got)-1] != want { // 15 + 25*0.5 = 27.5 -> 28 (округление к ближайшему)
		t.Fatalf("mid percent = %d, want %d", got[len(got)-1], want)
	}
	ReportFraction(ctx, "c", 1)
	if want := 40; got[len(got)-1] != want {
		t.Fatalf("end percent = %d, want %d", got[len(got)-1], want)
	}
	// Откат доли не должен уменьшать процент.
	ReportFraction(ctx, "d", 0.1)
	if last := got[len(got)-1]; last != 40 {
		t.Fatalf("percent went backwards: %d", last)
	}

	for i := 1; i < len(got); i++ {
		if got[i] < got[i-1] {
			t.Fatalf("percent not monotonic: %v", got)
		}
	}
}

// Точная доля done/total и обновление подписи.
func TestReportDoneTotal(t *testing.T) {
	var lastP int
	var lastLabel string
	ctx := WithProgress(context.Background(), 0, 100, func(p int, label string) {
		lastP, lastLabel = p, label
	})

	Report(ctx, "DNS: x.com (1/4)", 1, 4)
	if lastP != 25 || lastLabel != "DNS: x.com (1/4)" {
		t.Fatalf("got (%d,%q), want (25,%q)", lastP, lastLabel, "DNS: x.com (1/4)")
	}
	Report(ctx, "DNS: x.com (4/4)", 4, 4)
	if lastP != 100 {
		t.Fatalf("got %d, want 100", lastP)
	}
}

// Scope сужает диапазон, а восстановление возвращает прежний.
func TestScopeNarrowsAndRestores(t *testing.T) {
	var p int
	ctx := WithProgress(context.Background(), 0, 100, func(got int, _ string) { p = got })

	sctx, restore := Scope(ctx, 40, 50)
	ReportFraction(sctx, "inner", 1)
	if p != 50 {
		t.Fatalf("inner scope percent = %d, want 50", p)
	}
	restore()

	ReportFraction(ctx, "outer", 1)
	if p != 100 {
		t.Fatalf("outer scope percent = %d, want 100", p)
	}
}

// ReportCreep сходится к верхней границе, не превышая её.
func TestReportCreepConverges(t *testing.T) {
	max := -1
	calls := 0
	ctx := WithProgress(context.Background(), 15, 40, func(got int, _ string) {
		max = got
		calls++
	})

	for i := 0; i < 50; i++ {
		ReportCreep(ctx, "creep")
	}
	if max != 40 {
		t.Fatalf("creep converged to %d, want 40", max)
	}
	if calls != 50 {
		t.Fatalf("handler calls = %d, want 50", calls)
	}
}
