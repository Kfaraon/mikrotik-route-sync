package web

import (
	"context"
	"errors"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

func TestMikrotikHealthColdStartAndFreshCache(t *testing.T) {
	var mu sync.Mutex
	var calls int
	s := &Server{pingFn: func(ctx context.Context) error {
		mu.Lock()
		calls++
		mu.Unlock()
		return nil
	}}

	ok, known := s.mikrotikHealth(context.Background())
	if !ok || !known {
		t.Fatalf("холодный старт: ok=%v known=%v, want true/true", ok, known)
	}

	// Свежий кэш: ping не повторяется.
	ok, known = s.mikrotikHealth(context.Background())
	if !ok || !known {
		t.Fatalf("свежий кэш: ok=%v known=%v, want true/true", ok, known)
	}
	mu.Lock()
	n := calls
	mu.Unlock()
	if n != 1 {
		t.Fatalf("ping вызван %d раз(а), want 1", n)
	}
}

func TestMikrotikHealthPingError(t *testing.T) {
	s := &Server{pingFn: func(ctx context.Context) error {
		return errors.New("router down")
	}}

	ok, known := s.mikrotikHealth(context.Background())
	if known && ok {
		t.Fatal("ping упал: ожидалось ok=false")
	}
	if !known {
		t.Fatal("после ping состояние должно стать известным")
	}

	r := httptest.NewRequest("GET", "/", nil)
	class, title := s.dotState(r)
	if class != "live-dot live-off" {
		t.Fatalf("class = %q, want %q", class, "live-dot live-off")
	}
	if title != "MikroTik: нет подключения" {
		t.Fatalf("title = %q", title)
	}
}

func TestDotStateLive(t *testing.T) {
	s := &Server{pingFn: func(ctx context.Context) error { return nil }}
	r := httptest.NewRequest("GET", "/", nil)
	class, title := s.dotState(r)
	if class != "live-dot live-on" {
		t.Fatalf("class = %q, want %q", class, "live-dot live-on")
	}
	if title != "MikroTik: подключение есть" {
		t.Fatalf("title = %q", title)
	}
}

func TestMikrotikHealthStaleRefreshesInBackground(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	var mu sync.Mutex
	fail := false

	s := &Server{pingFn: func(ctx context.Context) error {
		mu.Lock()
		first := !fail
		mu.Unlock()
		if first {
			return nil
		}
		started <- struct{}{}
		<-release
		return errors.New("router down")
	}}

	// Холодный старт: ok=true, кэш становится устаревшим.
	if ok, known := s.mikrotikHealth(context.Background()); !ok || !known {
		t.Fatalf("холодный старт: ok=%v known=%v", ok, known)
	}
	s.mtMu.Lock()
	s.mtAt = time.Now().Add(-time.Minute)
	s.mtMu.Unlock()
	mu.Lock()
	fail = true
	mu.Unlock()

	// Устаревший кэш отдаётся сразу, обновление уходит в фон.
	done := make(chan struct{})
	go func() {
		defer close(done)
		if ok, known := s.mikrotikHealth(context.Background()); !ok || !known {
			t.Errorf("устаревший кэш: ok=%v known=%v, want true/true", ok, known)
		}
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("mikrotikHealth заблокировался на устаревшем кэше")
	}
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("фоновый ping не запустился")
	}

	// Пока обновление в полёте, повторный вызов не блокируется.
	if ok, known := s.mikrotikHealth(context.Background()); !ok || !known {
		t.Fatalf("refresh в полёте: ok=%v known=%v, want true/true", ok, known)
	}

	close(release)
	deadline := time.Now().Add(2 * time.Second)
	for {
		s.mtMu.Lock()
		updated := s.mtOK == false && s.mtKnown && !s.mtRefreshing
		s.mtMu.Unlock()
		if updated {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("фоновое обновление кэша не завершилось")
		}
		time.Sleep(10 * time.Millisecond)
	}
}
