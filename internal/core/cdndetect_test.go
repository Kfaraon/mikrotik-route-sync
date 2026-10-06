package core

// Тесты автоопределения источника CDN (cdndetect.go): пробник на фейковых
// deps, AdviseCDN (текущий метод/рекомендация), авто-детект при добавлении
// сервиса и применение рекомендации.

import (
	"context"
	"fmt"
	"net/http"
	"net/netip"
	"strings"
	"sync"
	"testing"

	"github.com/Kfaraon/mikrotik-route-sync/internal/addresslist"
	"github.com/Kfaraon/mikrotik-route-sync/internal/classifier"
	"github.com/Kfaraon/mikrotik-route-sync/internal/config"
	"github.com/Kfaraon/mikrotik-route-sync/internal/notifier"
	"github.com/Kfaraon/mikrotik-route-sync/internal/resolver"
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
			reason: "не разрешаются",
		},
		{
			name: "resolve empty",
			mutate: func(d *cdnProbeDeps) {
				d.resolveDomain = func(_ context.Context, _ string) ([]string, error) {
					return nil, nil
				}
			},
			reason: "не разрешаются",
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

// deadEndResolver — резолвер в никуда (быстрый отказ без сети):
// все DNS-запросы в тестах должны идти через стабы, а не в интернет.
func deadEndResolver() *resolver.Resolver {
	return resolver.NewResolver("127.0.0.1:1", nil, nil, 0).WithFallbacks("127.0.0.1:2")
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
			name: "bare unknown name -> probe on guessed domain", service: "yt",
			detected: true, wantRecommend: true,
			wantMethod:  "dynamic,whois",
			probeCalled: true,
		},
		{
			name: "bare known name -> probe on classifier domains", service: "instagram",
			detected: true, wantRecommend: true,
			wantMethod:  "dynamic,asn",
			probeCalled: true,
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
			// Офлайн: сбор подменён, резолвер в тупик, SNI выключена
			// (иначе тесты ходят в сеть).
			s.collectFn = func(_ context.Context, _ string, _ config.ServiceOverride) ([]netip.Prefix, string, error) {
				return []netip.Prefix{netip.MustParsePrefix("10.0.0.0/8")}, "whois", nil
			}
			s.resPtr.Store(deadEndResolver())
			off := false
			s.cfg.Safety.VerifySNI = &off

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
	if len(ov.Domains) != 1 || ov.Domains[0] != "rutracker.net" {
		t.Fatalf("override domains=%v, want [rutracker.net]", ov.Domains)
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
	if got := s.GetServiceOverride("yt").Domains; len(got) != 1 || got[0] != "yt.net" {
		t.Fatalf("domains=%v, want [yt.net]", got)
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

func TestServiceDomains(t *testing.T) {
	cases := []struct {
		name    string
		service string
		ov      config.ServiceOverride
		want    []string
	}{
		{
			name:    "arbitrary domain with explicit cdn falls back to service name",
			service: "example.net",
			ov:      config.ServiceOverride{Method: "cdn"},
			want:    []string{"example.net"},
		},
		{
			name:    "override domains win",
			service: "example.net",
			ov:      config.ServiceOverride{Method: "cdn", Domains: []string{"a.net", "b.net"}},
			want:    []string{"a.net", "b.net"},
		},
		{
			name:    "known service keeps classifier domains",
			service: "youtube",
			ov:      config.ServiceOverride{Method: "cdn"},
			want:    []string{"youtube.com", "googlevideo.com", "ytimg.com"},
		},
		{
			name:    "ip literal gives nothing",
			service: "8.8.8.8",
			ov:      config.ServiceOverride{Method: "whois"},
			want:    nil,
		},
		{
			name:    "unknown bare name falls back to guessed domain",
			service: "yt",
			ov:      config.ServiceOverride{Method: "whois"},
			want:    []string{"yt.com"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			class := classifier.Classify(tc.service, tc.ov.Method)
			got := serviceDomains(tc.service, tc.ov, class)
			if len(got) != len(tc.want) {
				t.Fatalf("domains=%v, want %v", got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Fatalf("domains=%v, want %v", got, tc.want)
				}
			}
		})
	}
}

func TestCurrentMethodNameAndSplit(t *testing.T) {
	if got := currentMethodName("x.net", config.ServiceOverride{Method: "cdn"}); got != "cdn" {
		t.Fatalf("override method=%q, want cdn", got)
	}
	if got := currentMethodName("rutracker.net", config.ServiceOverride{}); got != "whois" {
		t.Fatalf("default method=%q, want whois", got)
	}
	if got := currentMethodName("youtube", config.ServiceOverride{}); got != "dynamic,cdn" {
		t.Fatalf("youtube default=%q, want dynamic,cdn", got)
	}
	for _, s := range []string{"dynamic,whois", "dynamic whois", "dynamic;whois", " dynamic , whois ", "dynamic+cdn"} {
		parts := splitMethods(s)
		if len(parts) != 2 || parts[0] != "dynamic" || (parts[1] != "whois" && parts[1] != "cdn") {
			t.Fatalf("splitMethods(%q)=%v", s, parts)
		}
	}
}

func TestCDNDetectedMessage(t *testing.T) {
	adv := &CDNAdvice{CDN: "Cloudflare", ASN: 13335,
		URL: "https://www.cloudflare.com/ips-v4", PrefixCount: 15, WhoisCount: 86}
	msg := cdnDetectedMessage("rutracker.net", adv)
	for _, sub := range []string{"rutracker.net", "Cloudflare", "AS13335", "15", "86"} {
		if !strings.Contains(msg, sub) {
			t.Fatalf("message %q misses %q", msg, sub)
		}
	}

	advNoWhois := &CDNAdvice{CDN: "Cloudflare", ASN: 13335,
		URL: "https://www.cloudflare.com/ips-v4", PrefixCount: 15}
	if msg := cdnDetectedMessage("x.net", advNoWhois); strings.Contains(msg, "whois") {
		t.Fatalf("message without whois count must not mention whois: %q", msg)
	}
}

func TestCDNSwitchedMessage(t *testing.T) {
	adv := &CDNAdvice{CDN: "Cloudflare", ASN: 13335,
		URL: "https://www.cloudflare.com/ips-v4", PrefixCount: 15, WhoisCount: 86}
	msg := cdnSwitchedMessage("rutracker.net", adv, "whois", 86)
	for _, sub := range []string{"rutracker.net", "cdn", "Cloudflare", "AS13335", "whois", "15", "86"} {
		if !strings.Contains(msg, sub) {
			t.Fatalf("message %q misses %q", msg, sub)
		}
	}

	advNoWhois := &CDNAdvice{CDN: "Cloudflare", ASN: 13335,
		URL: "https://www.cloudflare.com/ips-v4", PrefixCount: 15}
	if msg := cdnSwitchedMessage("x.net", advNoWhois, "(auto)", 0); strings.Contains(msg, "Удалено") {
		t.Fatalf("message with purged=0 must not mention deletion: %q", msg)
	}
}

func TestFillQualityWiring(t *testing.T) {
	s, _ := newTestSyncer(t, noopRouter)
	s.cdnProbeFn = fakeProbe(true)
	// Сервис в конфиге, домен не резолвится (.invalid) — покрытие пустое,
	// но поле заполнено; сбор подменён, SNI выключена: всё офлайн.
	s.cfg.Services = append(s.cfg.Services, "nonexistent.invalid")
	s.collectFn = func(_ context.Context, _ string, _ config.ServiceOverride) ([]netip.Prefix, string, error) {
		return []netip.Prefix{netip.MustParsePrefix("10.0.0.0/8")}, "whois", nil
	}
	s.resPtr.Store(deadEndResolver())
	off := false
	s.cfg.Safety.VerifySNI = &off

	adv := s.AdviseCDN(context.Background(), "nonexistent.invalid", config.ServiceOverride{})
	if !adv.Detected {
		t.Fatalf("fake probe must detect: %q", adv.Reason)
	}
	if adv.Coverage == nil {
		t.Fatal("coverage must be filled for services in config")
	}
	if !adv.Coverage.Complete() {
		t.Fatalf("unresolvable domain must give complete report: %+v", adv.Coverage)
	}
	if adv.Verification != nil {
		t.Fatalf("sni disabled: verification=%v", adv.Verification)
	}
}

func TestRunCDNProbeSkipsUnresolvableDomain(t *testing.T) {
	deps := okDeps()
	deps.resolveDomain = func(_ context.Context, d string) ([]string, error) {
		if d == "ytimg.com" {
			return nil, nil // apex без A-записей — пропуск, не провал
		}
		return []string{"104.16.1.1"}, nil
	}
	adv := runCDNProbe(context.Background(), "youtube",
		[]string{"youtube.com", "ytimg.com"}, deps)
	if !adv.Detected {
		t.Fatalf("one dead domain must not kill detection: %q", adv.Reason)
	}
	if len(adv.Skipped) != 1 || adv.Skipped[0] != "ytimg.com" {
		t.Fatalf("skipped=%v", adv.Skipped)
	}
	if len(adv.IPs) != 1 {
		t.Fatalf("ips=%v", adv.IPs)
	}
}

func TestAdviseCDNPropagatesSkipped(t *testing.T) {
	s, _ := newTestSyncer(t, noopRouter)
	s.cdnProbeFn = func(_ context.Context, _ string, _ []string) *CDNAdvice {
		return &CDNAdvice{Detected: true, CDN: "Google", ASN: 15169,
			Skipped: []string{"ytimg.com"}, Reason: "fake"}
	}
	s.collectFn = func(_ context.Context, _ string, _ config.ServiceOverride) ([]netip.Prefix, string, error) {
		return nil, "", fmt.Errorf("offline")
	}
	s.resPtr.Store(deadEndResolver())
	off := false
	s.cfg.Safety.VerifySNI = &off

	adv := s.AdviseCDN(context.Background(), "youtube", config.ServiceOverride{})
	if !adv.Detected || len(adv.Skipped) != 1 || adv.Skipped[0] != "ytimg.com" {
		t.Fatalf("adv=%+v", adv)
	}
}
