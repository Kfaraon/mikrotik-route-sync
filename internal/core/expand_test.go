package core

// Тесты авто-совета по расширению доменного пресета до "preset,asn".

import (
	"context"
	"strings"
	"testing"

	"github.com/Kfaraon/mikrotik-route-sync/internal/config"
)

// presetOverrideInsta — типовой override доменного пресета Instagram.
func presetOverrideInsta() config.ServiceOverride {
	return config.ServiceOverride{
		Preset:  "instagram",
		Method:  "preset",
		Primary: "instagram.com",
	}
}

// TestAddPresetServiceAutoExpandConfig — при включённом presets.expand_method_auto
// добавление доменного пресет-сервиса сразу даёт method preset,asn + ASN.
func TestAddPresetServiceAutoExpandConfig(t *testing.T) {
	m := &mockRouter{t: t}
	s, cfg := newTestSyncer(t, m.serve(nil, false))
	cfg.Presets.ExpandMethodAuto = true
	s.expandCountFn = func(_ context.Context, _ int) (int, error) { return 240, nil }

	ov := config.ServiceOverride{Preset: "instagram", Method: "preset", Domains: []string{"instagram.com"}, Primary: "instagram.com"}
	if err := s.AddServiceWithConfig(context.Background(), "instagram.com", ov); err != nil {
		t.Fatalf("add: %v", err)
	}
	got := s.GetServiceOverride("instagram.com")
	if got.ASN != 32934 || got.Method != "preset,asn" {
		t.Fatalf("auto-expand not applied: %+v", got)
	}
}

// TestAddPresetServiceNoAutoExpandByDefault — без флага конфига метод остаётся
// просто preset (система только советует).
func TestAddPresetServiceNoAutoExpandByDefault(t *testing.T) {
	m := &mockRouter{t: t}
	s, _ := newTestSyncer(t, m.serve(nil, false))
	called := false
	s.expandCountFn = func(_ context.Context, _ int) (int, error) { called = true; return 240, nil }

	ov := config.ServiceOverride{Preset: "instagram", Method: "preset", Domains: []string{"instagram.com"}, Primary: "instagram.com"}
	if err := s.AddServiceWithConfig(context.Background(), "instagram.com", ov); err != nil {
		t.Fatalf("add: %v", err)
	}
	got := s.GetServiceOverride("instagram.com")
	if got.ASN != 0 || got.Method != "preset" {
		t.Fatalf("must stay preset by default: %+v", got)
	}
	if called {
		t.Fatal("ASN size must not be measured when auto-expand is off")
	}
}

// TestAdviseExpandDeclaredASN — объявленный узкий ASN предлагается к расширению.
func TestAdviseExpandDeclaredASN(t *testing.T) {
	s, _ := newTestSyncer(t, noopRouter)
	s.cdnProbeFn = func(_ context.Context, _ string, domains []string) *CDNAdvice {
		return &CDNAdvice{Domains: domains, Reason: "stub"}
	}
	s.expandCountFn = func(_ context.Context, asn int) (int, error) {
		if asn != 32934 {
			t.Fatalf("unexpected ASN %d", asn)
		}
		return 240, nil
	}

	adv := s.AdviseCDN(context.Background(), "instagram", presetOverrideInsta(), WithExpand())
	if !adv.ExpandRecommend {
		t.Fatalf("expected expand recommend, got reason=%q", adv.ExpandReason)
	}
	if adv.ExpandASN != 32934 || adv.ExpandPrefixes != 240 {
		t.Fatalf("expand=%d prefixes=%d", adv.ExpandASN, adv.ExpandPrefixes)
	}
}

// TestAdviseExpandTooWide — гигантский ASN не предлагается.
func TestAdviseExpandTooWide(t *testing.T) {
	s, _ := newTestSyncer(t, noopRouter)
	s.cdnProbeFn = func(_ context.Context, _ string, domains []string) *CDNAdvice {
		return &CDNAdvice{Domains: domains}
	}
	s.expandCountFn = func(_ context.Context, _ int) (int, error) { return 5000, nil }

	adv := s.AdviseCDN(context.Background(), "instagram", presetOverrideInsta(), WithExpand())
	if adv.ExpandRecommend {
		t.Fatal("too-wide ASN must not be recommended")
	}
	if adv.ExpandASN != 32934 || adv.ExpandPrefixes != 5000 {
		t.Fatalf("expand=%d prefixes=%d", adv.ExpandASN, adv.ExpandPrefixes)
	}
	if !strings.Contains(adv.ExpandReason, "широк") {
		t.Fatalf("reason=%q", adv.ExpandReason)
	}
}

// TestAdviseExpandAlreadyCurrent — уже preset,asn с ASN → совет не нужен.
func TestAdviseExpandAlreadyCurrent(t *testing.T) {
	s, _ := newTestSyncer(t, noopRouter)
	s.cdnProbeFn = func(_ context.Context, _ string, domains []string) *CDNAdvice {
		return &CDNAdvice{Domains: domains}
	}
	ov := presetOverrideInsta()
	ov.Method = "preset,asn"
	ov.ASN = 32934

	adv := s.AdviseCDN(context.Background(), "instagram", ov, WithExpand())
	if adv.ExpandRecommend {
		t.Fatal("already expanded must not recommend")
	}
	if !adv.ExpandCurrent {
		t.Fatal("expected ExpandCurrent")
	}
}

// TestAdviseExpandNoPreset — без доменного пресета совет не считается.
func TestAdviseExpandNoPreset(t *testing.T) {
	s, _ := newTestSyncer(t, noopRouter)
	s.cdnProbeFn = func(_ context.Context, _ string, domains []string) *CDNAdvice {
		return &CDNAdvice{Domains: domains}
	}
	adv := s.AdviseCDN(context.Background(), "rutracker.net", config.ServiceOverride{Method: "whois"})
	if adv.ExpandASN != 0 || adv.ExpandRecommend || adv.ExpandReason != "" {
		t.Fatalf("expand fields must be empty: %+v", adv)
	}
}

// TestApplyExpandRecommendation — применение выставляет ASN и method preset,asn.
func TestApplyExpandRecommendation(t *testing.T) {
	m := &mockRouter{t: t}
	s, _ := newTestSyncer(t, m.serve(nil, false))
	s.cfg.Services = append(s.cfg.Services, "instagram")
	s.cfg.Overrides = map[string]config.ServiceOverride{"instagram": presetOverrideInsta()}

	if err := s.ApplyExpandRecommendation(context.Background(), "instagram", 32934); err != nil {
		t.Fatalf("apply: %v", err)
	}
	ov := s.GetServiceOverride("instagram")
	if ov.ASN != 32934 || ov.Method != "preset,asn" {
		t.Fatalf("ov=%+v", ov)
	}
	if ov.Preset != "instagram" {
		t.Fatalf("preset lost: %+v", ov)
	}

	// Чужой явный ASN не переопределяем.
	s.cfg.Overrides["instagram"] = config.ServiceOverride{Preset: "instagram", Method: "preset,asn", ASN: 12345}
	if err := s.ApplyExpandRecommendation(context.Background(), "instagram", 32934); err == nil {
		t.Fatal("must refuse to override explicit ASN")
	}
}

// TestApplyExpandRecommendationNoPreset — сервис без доменного пресета расширять нечем.
func TestApplyExpandRecommendationNoPreset(t *testing.T) {
	m := &mockRouter{t: t}
	s, _ := newTestSyncer(t, m.serve(nil, false))
	s.cfg.Services = append(s.cfg.Services, "plain")
	s.cfg.Overrides = map[string]config.ServiceOverride{"plain": {Method: "whois"}}
	if err := s.ApplyExpandRecommendation(context.Background(), "plain", 32934); err == nil {
		t.Fatal("must refuse non-preset service")
	}
}

// TestApplyCDNRecommendationRefusesPreset — preset-сервис нельзя переключить на
// cdn: это снесло бы привязанный пресет.
func TestApplyCDNRecommendationRefusesPreset(t *testing.T) {
	m := &mockRouter{t: t}
	s, _ := newTestSyncer(t, m.serve(nil, false))
	s.cfg.Services = append(s.cfg.Services, "instagram")
	s.cfg.Overrides = map[string]config.ServiceOverride{"instagram": presetOverrideInsta()}

	adv := &CDNAdvice{Detected: true, CDN: "Cloudflare", URL: "https://example.com/cf", ASN: 13335}
	if _, err := s.ApplyCDNRecommendation(context.Background(), "instagram", adv); err == nil {
		t.Fatal("must refuse to switch a preset service to cdn")
	}
	ov := s.GetServiceOverride("instagram")
	if ov.Method != "preset" {
		t.Fatalf("preset service mutated: %+v", ov)
	}
}
