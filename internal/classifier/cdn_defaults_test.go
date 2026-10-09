package classifier

import (
	"testing"

	"github.com/Kfaraon/mikrotik-route-sync/internal/config"
)

// TestCDNServicesCoveredByDefaultSources страхует рассинхрон: каждый
// CDN-сервис без доменов (метод cdn) обязан иметь запись в дефолтном каталоге
// config.cdn_sources. Иначе сервис в рантайме не найдёт ASN/URL и упадёт.
func TestCDNServicesCoveredByDefaultSources(t *testing.T) {
	byService := map[string]bool{}
	for _, s := range config.DefaultCDNSources() {
		byService[s.Service] = true
	}
	for name, k := range knownServices {
		if !contains(k.methods, "cdn") || len(k.domains) > 0 {
			continue
		}
		if !byService[name] {
			t.Errorf("CDN-сервис %q (метод cdn без доменов) отсутствует в config.DefaultCDNSources()", name)
		}
	}
}
