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
	"strings"
	"testing"
	"time"
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
