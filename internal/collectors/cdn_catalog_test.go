package collectors

import (
	"testing"

	"github.com/Kfaraon/mikrotik-route-sync/internal/config"
)

// TestCDNSourcesConfigurable проверяет, что каталог CDN управляется из конфига
// (SetCDNSources), lookup идёт и по ASN, и по имени сервиса-алиаса.
func TestCDNSourcesConfigurable(t *testing.T) {
	// Восстанавливаем дефолтный каталог после теста (глобальный снимок).
	defer SetCDNSources(config.DefaultCDNSources())

	SetCDNSources([]config.CDNSourceConfig{
		{Service: "custom", Name: "Custom CDN", ASN: 64500, URL: "https://example.com/list.txt", Parser: "text"},
	})

	asn, name, url, ok := CDNSourceByName("custom")
	if !ok || asn != 64500 || name != "Custom CDN" || url != "https://example.com/list.txt" {
		t.Fatalf("CDNSourceByName(custom) = %d,%q,%q,%v", asn, name, url, ok)
	}
	// Регистронезависимый lookup по алиасу.
	if _, _, _, ok := CDNSourceByName("CUSTOM"); !ok {
		t.Fatal("CDNSourceByName должен быть регистронезависимым")
	}
	// Базовый CDN (cloudflare) больше не должен находиться — каталог заменён.
	if _, _, _, ok := CDNSourceByName("cloudflare"); ok {
		t.Fatal("старый каталог не должен оставаться после SetCDNSources")
	}
	if _, _, ok := CDNSource(13335); ok {
		t.Fatal("CDNSource(13335) не должен находиться после замены каталога")
	}
	// Замена каталога вернула базовые значения.
	SetCDNSources(config.DefaultCDNSources())
	if got, _, _, ok := CDNSourceByName("cloudflare"); !ok || got != 13335 {
		t.Fatalf("после сброса CDNSourceByName(cloudflare) = %d,%v, want 13335", got, ok)
	}
}
