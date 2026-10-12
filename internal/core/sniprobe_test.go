package core

// Тесты SNI-пробы (sniprobe.go): позитив — локальный TLS-сервер с доверенным
// в тесте сертификатом; негатив — dial-заглушка с ошибкой. Внешняя сеть
// не используется.

import (
	"context"
	"crypto/x509"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Kfaraon/mikrotik-route-sync/internal/notifier"
)

func testVerifier(t *testing.T, ts *httptest.Server) *SNIVerifier {
	t.Helper()
	pool := x509.NewCertPool()
	pool.AddCert(ts.Certificate())
	return &SNIVerifier{
		Timeout: 5 * time.Second,
		Roots:   pool,
		Dial: func(_ context.Context, _, _ string) (net.Conn, error) {
			return net.Dial("tcp", ts.Listener.Addr().String())
		},
	}
}

func TestSNIVerifyLocalTLSServer(t *testing.T) {
	ts := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {}))
	defer ts.Close()

	// Сертификат httptest валиден для example.com.
	vd := testVerifier(t, ts).Verify(context.Background(), "example.com", []string{"203.0.113.7"})
	if !vd.Passed() || vd.Checked != 1 || vd.OK != 1 {
		t.Fatalf("verdict=%+v", vd)
	}
}

func TestSNIVerifyWrongDomainFails(t *testing.T) {
	ts := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {}))
	defer ts.Close()

	vd := testVerifier(t, ts).Verify(context.Background(), "other.example", []string{"203.0.113.7"})
	if vd.Passed() || len(vd.Failed) != 1 {
		t.Fatalf("wrong domain must fail: %+v", vd)
	}
}

func TestSNIVerifyDialError(t *testing.T) {
	v := &SNIVerifier{
		Timeout: 2 * time.Second,
		Dial: func(_ context.Context, _, _ string) (net.Conn, error) {
			return nil, fmt.Errorf("connection refused")
		},
	}
	vd := v.Verify(context.Background(), "x.net", []string{"10.0.0.1", "10.0.0.2"})
	if vd.Passed() || vd.Checked != 2 || len(vd.Failed) != 2 {
		t.Fatalf("verdict=%+v", vd)
	}
}

func TestSNIVerdictPassed(t *testing.T) {
	var nilVd *SNIVerdict
	if !nilVd.Passed() {
		t.Fatal("nil verdict must pass")
	}
	if !(&SNIVerdict{Domain: "x"}).Passed() {
		t.Fatal("empty verdict must pass")
	}
	if (&SNIVerdict{Domain: "x", Checked: 2, OK: 1, Failed: []string{"1.1.1.1"}}).Passed() {
		t.Fatal("verdict with failures must not pass")
	}
}

func TestSNIAlertMessage(t *testing.T) {
	msg := sniAlertMessage("x.net", []*SNIVerdict{
		{Domain: "x.net", Checked: 2, OK: 1, Failed: []string{"10.0.0.9"}},
	})
	for _, sub := range []string{"x.net", "SNI", "10.0.0.9"} {
		if !strings.Contains(msg, sub) {
			t.Fatalf("alert %q misses %q", msg, sub)
		}
	}
}

func TestSniFailedDomains(t *testing.T) {
	vds := []*SNIVerdict{
		{Domain: "a", Checked: 1, OK: 1},
		{Domain: "b", Checked: 2, OK: 0, Failed: []string{"1.1.1.1", "2.2.2.2"}},
		{Domain: "c"}, // нечего проверять — не провал
		nil,
	}
	got := sniFailedDomains(vds)
	if len(got) != 1 || got[0].Domain != "b" {
		t.Fatalf("failed=%v", got)
	}
}

func TestPrimarySNIFailed(t *testing.T) {
	pass := &SNIVerdict{Domain: "youtube.com", Checked: 2, OK: 2}
	fail := &SNIVerdict{Domain: "googlevideo.com", Checked: 3, OK: 1, Failed: []string{"1.2.3.4", "5.6.7.8"}}
	empty := &SNIVerdict{Domain: "ytimg.com"} // не резолвится — не сигнал

	// Primary прошёл, aux упал — тихо (видно только в Verification).
	if got := primarySNIFailed([]*SNIVerdict{pass, fail}); len(got) != 0 {
		t.Fatalf("aux-only failure must be silent, got %v", got)
	}
	// Primary упал — вердикт только по нему.
	if got := primarySNIFailed([]*SNIVerdict{fail, pass}); len(got) != 1 || got[0].Domain != "googlevideo.com" {
		t.Fatalf("got %v", got)
	}
	// Пустой первый вердикт пропускается, primary — следующий проверяемый.
	if got := primarySNIFailed([]*SNIVerdict{empty, fail}); len(got) != 1 {
		t.Fatalf("got %v", got)
	}
	if got := primarySNIFailed(nil); len(got) != 0 {
		t.Fatalf("got %v", got)
	}
}

// H10: метка троттлинга SNI-алерта ставится только после успешной отправки,
// иначе сбой Send подавляет повторный алерт на сутки.
func TestSNIAlertMarksOnlyAfterSuccess(t *testing.T) {
	s, _ := newTestSyncer(t, noopRouter)
	n := &flakyNotifier{failFirst: true}
	s.SetNotifier(n)
	ctx := context.Background()
	vds := []*SNIVerdict{{Domain: "x.net", Checked: 1, OK: 0, Failed: []string{"1.1.1.1"}}}

	// Первая попытка — Send падает: метка НЕ должна быть поставлена.
	s.maybeAlertSNI(ctx, "x.net", vds)
	if _, ok := s.sniAlerted["x.net"]; ok {
		t.Fatal("alert marked despite send failure")
	}
	// Вторая — Send успешен: метка поставлена.
	s.maybeAlertSNI(ctx, "x.net", vds)
	if _, ok := s.sniAlerted["x.net"]; !ok {
		t.Fatal("alert not marked after success")
	}
	// Третья — троттлинг: Send больше не вызывается.
	before := n.calls
	s.maybeAlertSNI(ctx, "x.net", vds)
	if n.calls != before {
		t.Fatalf("throttle broken: calls=%d", n.calls)
	}
}

// TestSNIVerifyRespectsContextCancel — при отмене контекста новые пробы не
// запускаются: иначе цикл продолжает захватывать семафор и слать запросы по
// отменённому контексту (висящие горутины после остановки синка).
func TestSNIVerifyRespectsContextCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())

	var mu sync.Mutex
	var dials int
	var wg sync.WaitGroup
	wg.Add(1)

	v := &SNIVerifier{
		Timeout: 5 * time.Second,
		Dial: func(dctx context.Context, _, _ string) (net.Conn, error) {
			mu.Lock()
			dials++
			mu.Unlock()
			<-dctx.Done() // «зависший» dial, освобождается только отменой
			return nil, dctx.Err()
		},
	}

	ips := make([]string, 100)
	for i := range ips {
		ips[i] = "10.0.0." + strconv.Itoa(i+1)
	}

	go func() {
		defer wg.Done()
		v.Verify(ctx, "example.com", ips)
	}()

	time.Sleep(50 * time.Millisecond)
	cancel()

	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("Verify did not return after context cancellation")
	}

	mu.Lock()
	defer mu.Unlock()
	if dials >= len(ips) {
		t.Fatalf("all probes launched despite cancellation: %d", dials)
	}
}

// TestSNIVerifyCheckedConsistent — Checked всегда равен OK + число провалов:
// прогресс и метрики опираются на это равенство.
func TestSNIVerifyCheckedConsistent(t *testing.T) {
	v := &SNIVerifier{
		Timeout: 2 * time.Second,
		Dial: func(_ context.Context, _, addr string) (net.Conn, error) {
			if strings.HasSuffix(addr, ":443") {
				return nil, fmt.Errorf("refused")
			}
			return nil, fmt.Errorf("refused")
		},
	}
	ips := []string{"10.0.0.1", "10.0.0.2", "10.0.0.3"}
	vd := v.Verify(context.Background(), "x.net", ips)
	if vd.Checked != vd.OK+len(vd.Failed) {
		t.Fatalf("inconsistent verdict: %+v", vd)
	}
	if vd.Checked != 3 {
		t.Fatalf("checked=%d want 3", vd.Checked)
	}
}

// TestSniCacheEvictsExpired — при переполнении просроченные вердикты
// вычищаются, карта не растёт неограниченно.
func TestSniCacheEvictsExpired(t *testing.T) {
	s, _ := newTestSyncer(t, noopRouter)

	s.sniMu.Lock()
	for i := 0; i < sniCacheMaxEntries; i++ {
		s.sniCache["expired-"+strconv.Itoa(i)] = sniCacheEntry{expires: time.Now().Add(-time.Minute)}
	}
	// Одна живая запись — не должна быть вытеснена.
	s.sniCache["live"] = sniCacheEntry{expires: time.Now().Add(time.Hour)}
	s.sniMu.Unlock()

	s.sniCachePut("fresh", sniCacheEntry{expires: time.Now().Add(time.Hour)})

	s.sniMu.Lock()
	size := len(s.sniCache)
	_, liveKept := s.sniCache["live"]
	_, freshKept := s.sniCache["fresh"]
	s.sniMu.Unlock()

	if !liveKept || !freshKept {
		t.Fatalf("live/fresh entries must survive eviction (live=%v fresh=%v)", liveKept, freshKept)
	}
	if size > sniCacheMaxEntries {
		t.Fatalf("cache not evicted: size=%d", size)
	}
}

// TestSniCacheGetDeletesExpired — просроченный вердикт удаляется при чтении.
func TestSniCacheGetDeletesExpired(t *testing.T) {
	s, _ := newTestSyncer(t, noopRouter)
	s.sniMu.Lock()
	s.sniCache["x.net"] = sniCacheEntry{expires: time.Now().Add(-time.Second)}
	s.sniMu.Unlock()

	if _, ok := s.sniCacheGet("x.net"); ok {
		t.Fatal("expired entry must miss")
	}
	s.sniMu.Lock()
	_, still := s.sniCache["x.net"]
	s.sniMu.Unlock()
	if still {
		t.Fatal("expired entry must be deleted on read")
	}
}

type flakyNotifier struct {
	failFirst bool
	sent      bool
	calls     int
}

func (f *flakyNotifier) Send(context.Context, string) error {
	f.calls++
	if f.failFirst && !f.sent {
		f.sent = true
		return fmt.Errorf("send failed")
	}
	return nil
}
func (f *flakyNotifier) SyncStart(context.Context, []string, string, string) error { return nil }
func (f *flakyNotifier) SyncDone(context.Context, []notifier.SyncResult, any, bool) error {
	return nil
}
func (f *flakyNotifier) Error(context.Context, string, error) error { return nil }

// verifyServiceSNIProgress сообщает шаги по доменам для UI-прогресса; кэш не
// мешает сосчитать шаги (колбэк вызывается до обращения к кэшу).
func TestVerifyServiceSNIProgressReportsSteps(t *testing.T) {
	s, _ := newTestSyncer(t, noopRouter)

	// Резолвер в никуда: домены не резолвятся, вердикты пустые, но шаги идут.
	s.resPtr.Store(deadEndResolver())

	type step struct {
		done, total int
		domain      string
	}
	var steps []step
	s.verifyServiceSNIProgress(context.Background(), []string{"a.net", "b.net", "c.net"},
		func(done, total int, domain string) {
			steps = append(steps, step{done, total, domain})
		})

	if len(steps) != 3 {
		t.Fatalf("expected 3 steps, got %+v", steps)
	}
	for i, want := range []string{"a.net", "b.net", "c.net"} {
		if steps[i].done != i || steps[i].total != 3 || steps[i].domain != want {
			t.Fatalf("step %d: %+v", i, steps[i])
		}
	}
}
