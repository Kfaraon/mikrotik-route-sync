package collectors

import (
	"context"
	"sync"
)

// ProgressFunc получает абсолютный процент (в общем диапазоне фазы сбора) и
// текстовую подпись текущего подшага. Вызывается best-effort: не влияет на сбор.
type ProgressFunc func(percent int, label string)

type progressCtxKey struct{}

// progressCtx — носитель текущего диапазона [lo, hi] и последнего процента.
// Процент только растёт: подшаги отчитываются не по порядку, а строка в UI
// не должна отпрыгивать назад.
type progressCtx struct {
	mu   sync.Mutex
	lo   int
	hi   int
	last int
	fn   ProgressFunc
}

// WithProgress привязывает к контексту приёмник прогресса с диапазоном
// [lo, hi] (абсолютные проценты фазы сбора).
func WithProgress(ctx context.Context, lo, hi int, fn ProgressFunc) context.Context {
	if fn == nil {
		return ctx
	}
	if hi < lo {
		hi = lo
	}
	return context.WithValue(ctx, progressCtxKey{}, &progressCtx{lo: lo, hi: hi, last: lo, fn: fn})
}

// Scope временно сужает диапазон прогресса (например, для одного метода
// сбора), чтобы доли подшага отображались в его собственный отрезок общего
// диапазона. Возвращает контекст и функцию восстановления прежнего диапазона.
// Сбор идёт последовательно, поэтому смена диапазона безопасна.
func Scope(ctx context.Context, lo, hi int) (context.Context, func()) {
	pc, _ := ctx.Value(progressCtxKey{}).(*progressCtx)
	if pc == nil {
		return ctx, func() {}
	}
	if hi < lo {
		hi = lo
	}
	pc.mu.Lock()
	prevLo, prevHi := pc.lo, pc.hi
	pc.lo, pc.hi = lo, hi
	pc.mu.Unlock()
	return ctx, func() {
		pc.mu.Lock()
		pc.lo, pc.hi = prevLo, prevHi
		pc.mu.Unlock()
	}
}

// Report сообщает подшаг с точной долей (done/total) внутри текущего диапазона.
// total <= 0 — объём неизвестен, процент медленно подтягивается к верхней
// границе, но подпись всё равно обновляется.
func Report(ctx context.Context, label string, done, total int) {
	if total > 0 {
		if done < 0 {
			done = 0
		}
		if done > total {
			done = total
		}
		reportFraction(ctx, label, float64(done)/float64(total))
		return
	}
	ReportCreep(ctx, label)
}

// ReportFraction сообщает долю [0..1] внутри текущего диапазона (для потоковой
// загрузки, когда общий объём заранее неизвестен).
func ReportFraction(ctx context.Context, label string, frac float64) {
	reportFraction(ctx, label, frac)
}

// ReportCreep сдвигает процент на треть остатка диапазона (шаг без измеримого
// объёма). Повторные вызовы сходятся к верхней границе.
func ReportCreep(ctx context.Context, label string) {
	pc, _ := ctx.Value(progressCtxKey{}).(*progressCtx)
	if pc == nil {
		return
	}

	pc.mu.Lock()
	if pc.last < pc.hi {
		step := (pc.hi - pc.last) / 3
		if step < 1 {
			step = 1
		}
		pc.last += step
	}
	if pc.last > pc.hi {
		pc.last = pc.hi
	}
	pct := pc.last
	fn := pc.fn
	pc.mu.Unlock()

	fn(pct, label)
}

func reportFraction(ctx context.Context, label string, frac float64) {
	pc, _ := ctx.Value(progressCtxKey{}).(*progressCtx)
	if pc == nil {
		return
	}
	if frac < 0 {
		frac = 0
	}
	if frac > 1 {
		frac = 1
	}

	pc.mu.Lock()
	target := pc.lo + int(float64(pc.hi-pc.lo)*frac+0.5)
	if target < pc.last {
		target = pc.last
	}
	if target > pc.hi {
		target = pc.hi
	}
	pc.last = target
	pct := target
	fn := pc.fn
	pc.mu.Unlock()

	fn(pct, label)
}
