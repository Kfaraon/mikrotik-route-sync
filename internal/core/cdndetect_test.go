package core

// Тесты автоопределения источника CDN (cdndetect.go): пробник на фейковых
// deps, AdviseCDN (текущий метод/рекомендация), авто-детект при добавлении
// сервиса и применение рекомендации.

import (
	"context"
	"net/http"
	"net/netip"
	"strings"
	"sync"
	"testing"

	"github.com/Kfaraon/mikrotik-route-sync/internal/addresslist"
	"github.com/Kfaraon/mikrotik-route-sync/internal/config"
	"github.com/Kfaraon/mikrotik-route-sync/internal/notifier"
)

// captureNotifier — захватывает отправленные уведомления.
type captureNotifier struct {
	notifier.NoopNotifier
	mu   sync.Mutex
	msgs []string
}

func (c *captureNotifier) Send(_ context.Context, message string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.msgs = append(c.msgs, message)
	return nil
}

func (c *captureNotifier) all() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.msgs...)
}

// noopRouter — обработчик, не обращающийся к MikroTik (для тестов без синка).
func noopRouter(http.ResponseWriter, *http.Request) {}

// okDeps — deps, где домен за Cloudflare: A-запись входит в список, ASN 13335.
func okDeps() *cdnProbeDeps {
	return &cdnProbeDeps{
		resolveDomain: func(_ context.Context, _ string) ([]string, error) {
			return []string{"104.16.1.1"}, nil
		},
		resolveASN: func(_ context.Context, _ string) (int, error) {
			return 13335, nil
		},
		collectCDN: func(_ context.Context, _ int) ([]netip.Prefix, string, error) {
			return []netip.Prefix{
				netip.MustParsePrefix("104.16.0.0/16"),
				netip.MustParsePrefix("172.64.0.0/16"),
			}, "https://www.cloudflare.com/ips-v4", nil
		},
		collectWhois: func(_ context.Context, _ []string) (int, error) {
			return 86, nil
		},
	}
}

func TestRunCDNProbeDetected(t *testing.T) {
	adv := runCDNProbe(context.Background(), "rutracker.net", []string{"rutracker.net"}, okDeps())

	if !adv.Detected {
		t.Fatalf("expected detected, got reason=%q", adv.Reason)
	}
	if adv.CDN != "Cloudflare" || adv.ASN != 13335 {
		t.Fatalf("wrong cdn/asn: %q AS%d", adv.CDN, adv.ASN)
	}
	if adv.URL != "https://www.cloudflare.com/ips-v4" {
		t.Fatalf("wrong url: %q", adv.URL)
	}
	if adv.PrefixCount != 2 || adv.WhoisCount != 86 {
		t.Fatalf("wrong counts: cdn=%d whois=%d", adv.PrefixCount, adv.WhoisCount)
	}
	if len(adv.IPs) != 1 || adv.IPs[0] != "104.16.1.1" {
		t.Fatalf("wrong ips: %v", adv.IPs)
	}
}

func TestRunCDNProbeWhoisCountBestEffort(t *testing.T) {
	deps := okDeps()
	deps.collectWhois = func(_ context.Context, _ []string) (int, error) {
		return 0, context.DeadlineExceeded
	}
	adv := runCDNProbe(context.Background(), "x.net", []string{"x.net"}, deps)
	if !adv.Detected {
		t.Fatalf("whois failure must not break detection: %q", adv.Reason)
	}
	if adv.WhoisCount != 0 {
		t.Fatalf("whois count should be 0, got %d", adv.WhoisCount)
	}
}

func TestRunCDNProbeNotDetected(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(d *cdnProbeDeps)
		reason string // подстрока ожидаемой причины
	}{
		{
			name: "resolve failed",
			mutate: func(d *cdnProbeDeps) {
				d.resolveDomain = func(_ context.Context, _ string) ([]string, error) {
					return nil, context.DeadlineExceeded
				}
			},
			reason: "не разрешается",
		},
		{
			name: "resolve empty",
			mutate: func(d *cdnProbeDeps) {
				d.resolveDomain = func(_ context.Context, _ string) ([]string, error) {
					return nil, nil
				}
			},
			reason: "не разрешается",
		},
		{
			name: "asn failed",
			mutate: func(d *cdnProbeDeps) {
				d.resolveASN = func(_ context.Context, _ string) (int, error) {
					return 0, context.DeadlineExceeded
				}
			},
			reason: "ASN домена",
		},
		{
			name: "asn not a known cdn",
			mutate: func(d *cdnProbeDeps) {
				d.resolveASN = func(_ context.Context, _ string) (int, error) {
					return 47764, nil
				}
			},
			reason: "не входит в список поддерживаемых CDN",
		},
		{
			name: "cdn list unavailable",
			mutate: func(d *cdnProbeDeps) {
				d.collectCDN = func(_ context.Context, _ int) ([]netip.Prefix, string, error) {
					return nil, "", context.DeadlineExceeded
				}
			},
			reason: "официальный список CDN недоступен",
		},
		{
			name: "ip outside list",
			mutate: func(d *cdnProbeDeps) {
				d.resolveDomain = func(_ context.Context, _ string) ([]string, error) {
					return []string{"193.46.255.29"}, nil
				}
			},
			reason: "вне списка CDN",
		},
		{
			name: "only ipv6",
			mutate: func(d *cdnProbeDeps) {
				d.resolveDomain = func(_ context.Context, _ string) ([]string, error) {
					return []string{"2606:4700::1"}, nil
				}
			},
			reason: "IPv4-адреса не найдены",
		},
		{
			name: "different asns across domains",
			mutate: func(d *cdnProbeDeps) {
				var i int
				d.resolveASN = func(_ context.Context, _ string) (int, error) {
					i++
					if i == 1 {
						return 13335, nil
					}
					return 16509, nil
				}
			},
			reason: "домены в разных AS",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			deps := okDeps()
			tc.mutate(deps)

			cdnCalled := false
			origCDN := deps.collectCDN
			deps.collectCDN = func(ctx context.Context, asn int) ([]netip.Prefix, string, error) {
				cdnCalled = true
				return origCDN(ctx, asn)
			}

			adv := runCDNProbe(context.Background(), "x.net", []string{"x.net", "y.net"}, deps)
			if adv.Detected {
				t.Fatalf("expected not detected, got %+v", adv)
			}
			if !strings.Contains(adv.Reason, tc.reason) {
				t.Fatalf("reason %q does not contain %q", adv.Reason, tc.reason)
			}
			if tc.name == "asn not a known cdn" && cdnCalled {
				t.Fatal("cdn list must not be fetched for non-CDN ASN")
			}
		})
	}
}

// fakeProbe возвращает детектирующий/недетектирующий пробник.
func fakeProbe(detected bool) func(context.Context, string, []string) *CDNAdvice {
	return func(_ context.Context, _ string, domains []string) *CDNAdvice {
		adv := &CDNAdvice{Domains: domains}
		if !detected {
			adv.Reason = "fake: not detected"
			return adv
		}
		adv.Detected = true
		adv.CDN = "Cloudflare"
		adv.ASN = 13335
		adv.URL = "https://www.cloudflare.com/ips-v4"
		adv.PrefixCount = 15
		adv.WhoisCount = 86
		adv.IPs = []string{"104.16.1.1"}
		adv.Reason = "все A-записи входят в официальный список https://www.cloudflare.com/ips-v4"
		return adv
	}
}

func TestAdviseCDNRecommendMatrix(t *testing.T) {
	cases := []struct {
		name          string
		service       string
		ov            config.ServiceOverride
		detected      bool
		wantRecommend bool
		wantMethod    string
		probeCalled   bool
	}{
		{
			name: "default whois + detected -> recommend", service: "rutracker.net",
			detected: true, wantRecommend: true, wantMethod: "whois", probeCalled: true,
		},
		{
			name: "explicit whois + detected -> recommend", service: "rutracker.net",
			ov:       config.ServiceOverride{Method: "whois"},
			detected: true, wantRecommend: true, wantMethod: "whois", probeCalled: true,
		},
		{
			name: "method cdn -> already optimal", service: "rutracker.net",
			ov:       config.ServiceOverride{Method: "cdn"},
			detected: true, wantRecommend: false, wantMethod: "cdn", probeCalled: true,
		},
		{
			name: "static_url same official list -> already optimal", service: "rutracker.net",
			ov: config.ServiceOverride{
				Method:    "static_url",
				StaticURL: "https://www.cloudflare.com/ips-v4",
			},
			detected: true, wantRecommend: false, wantMethod: "static_url", probeCalled: true,
		},
		{
			name: "static_url custom -> no replacement", service: "rutracker.net",
			ov: config.ServiceOverride{
				Method:    "static_url",
				StaticURL: "https://example.org/list.txt",
			},
			detected: true, wantRecommend: false, wantMethod: "static_url", probeCalled: true,
		},
		{
			name: "not detected -> no recommend", service: "rutracker.net",
			detected: false, wantRecommend: false, wantMethod: "whois", probeCalled: true,
		},
		{
			name: "ip literal -> probe skipped", service: "8.8.8.8",
			detected: true, wantRecommend: false, wantMethod: "whois", probeCalled: false,
		},
		{
			name: "name without dot -> probe skipped", service: "instagram",
			detected: true, wantRecommend: false,
			wantMethod:  "dynamic,asn",
			probeCalled: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s, _ := newTestSyncer(t, noopRouter)
			called := false
			s.cdnProbeFn = func(ctx context.Context, name string, domains []string) *CDNAdvice {
				called = true
				return fakeProbe(tc.detected)(ctx, name, domains)
			}

			adv := s.AdviseCDN(context.Background(), tc.service, tc.ov)
			if called != tc.probeCalled {
				t.Fatalf("probe called=%v, want %v", called, tc.probeCalled)
			}
			if adv.Recommend != tc.wantRecommend {
				t.Fatalf("Recommend=%v, want %v (reason=%q)", adv.Recommend, tc.wantRecommend, adv.Reason)
			}
			if adv.CurrentMethod != tc.wantMethod {
				t.Fatalf("CurrentMethod=%q, want %q", adv.CurrentMethod, tc.wantMethod)
			}
		})
	}
}

func TestAddServiceAutoDetectsCDN(t *testing.T) {
	s, _ := newTestSyncer(t, noopRouter)
	n := &captureNotifier{}
	s.SetNotifier(n)
	s.cdnProbeFn = fakeProbe(true)

	if err := s.AddServiceWithConfig(context.Background(), "rutracker.net", config.ServiceOverride{}); err != nil {
		t.Fatalf("add: %v", err)
	}

	ov := s.GetServiceOverride("rutracker.net")
	if ov.Method != "cdn" {
		t.Fatalf("override method=%q, want cdn", ov.Method)
	}

	msgs := n.all()
	if len(msgs) != 1 || !strings.Contains(msgs[0], "Cloudflare") || !strings.Contains(msgs[0], "15") {
		t.Fatalf("wrong notification: %v", msgs)
	}
}

func TestAddServiceExplicitMethodSkipsDetection(t *testing.T) {
	s, _ := newTestSyncer(t, noopRouter)
	called := false
	s.cdnProbeFn = func(ctx context.Context, name string, domains []string) *CDNAdvice {
		called = true
		return fakeProbe(true)(ctx, name, domains)
	}

	err := s.AddServiceWithConfig(context.Background(), "rutracker.net",
		config.ServiceOverride{Method: "whois"})
	if err != nil {
		t.Fatalf("add: %v", err)
	}
	if called {
		t.Fatal("probe must not run when method is explicit")
	}
	if got := s.GetServiceOverride("rutracker.net").Method; got != "whois" {
		t.Fatalf("method=%q, want whois", got)
	}
}

func TestAddServiceDetectionNotDetectedKeepsDefault(t *testing.T) {
	s, _ := newTestSyncer(t, noopRouter)
	s.SetNotifier(&captureNotifier{})
	s.cdnProbeFn = fakeProbe(false)

	if err := s.AddServiceWithConfig(context.Background(), "example.org", config.ServiceOverride{}); err != nil {
		t.Fatalf("add: %v", err)
	}
	if got := s.GetServiceOverride("example.org").Method; got != "" {
		t.Fatalf("method=%q, want empty (default)", got)
	}
}

func TestAddServiceInvalidMethodRejected(t *testing.T) {
	s, _ := newTestSyncer(t, noopRouter)
	err := s.AddServiceWithConfig(context.Background(), "example.org",
		config.ServiceOverride{Method: "bogus"})
	if err == nil || !strings.Contains(err.Error(), "unknown method") {
		t.Fatalf("expected unknown method error, got %v", err)
	}
}

func TestApplyCDNRecommendation(t *testing.T) {
	entries := []addresslist.Entry{
		{ID: "*1", Address: "8.8.8.0/24", List: "TO-VPN", Comment: "AUTO:yt"},
		{ID: "*2", Address: "1.1.1.0/24", List: "TO-VPN", Comment: "AUTO:yt"},
		{ID: "*3", Address: "9.9.9.0/24", List: "TO-VPN", Comment: "AUTO:other"},
	}
	m := &mockRouter{t: t}
	s, _ := newTestSyncer(t, m.serve(entries, false))
	s.SetNotifier(&captureNotifier{})

	adv := fakeProbe(true)(context.Background(), "yt", []string{"yt.net"})

	purged, err := s.ApplyCDNRecommendation(context.Background(), "yt", adv)
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if purged != 2 {
		t.Fatalf("purged=%d, want 2", purged)
	}
	if got := s.GetServiceOverride("yt").Method; got != "cdn" {
		t.Fatalf("method=%q, want cdn", got)
	}
	if len(m.deleted) != 2 {
		t.Fatalf("router deletes=%v, want 2 AUTO:yt entries", m.deleted)
	}

	// повторное применение — ошибка (уже cdn)
	if _, err := s.ApplyCDNRecommendation(context.Background(), "yt", adv); err == nil ||
		!strings.Contains(err.Error(), "already uses method cdn") {
		t.Fatalf("expected already-uses error, got %v", err)
	}
}

func TestApplyCDNRecommendationErrors(t *testing.T) {
	s, _ := newTestSyncer(t, noopRouter)

	// не обнаружен — применять нечего
	if _, err := s.ApplyCDNRecommendation(context.Background(), "yt", &CDNAdvice{}); err == nil {
		t.Fatal("expected error for not-detected advice")
	}

	// сервис отсутствует в конфиге
	if _, err := s.ApplyCDNRecommendation(context.Background(), "nosuch",
		fakeProbe(true)(context.Background(), "nosuch", nil)); err == nil ||
		!strings.Contains(err.Error(), "not found") {
		t.Fatalf("expected not-found error, got %v", err)
	}
}
