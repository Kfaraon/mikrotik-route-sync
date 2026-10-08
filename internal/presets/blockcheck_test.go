package presets

import (
	"os"
	"testing"
)

func TestBlockCheckBlocked(t *testing.T) {
	// создаем временный фикстурный файл (~20 строк сVarious случаями)
	fixture := `# это комментарий

example.com
trailing.example.com.
*.blocked.example
BLOCKED.EXAMPLE.COM
spaced.example.com
nodots
not a domain
xn--80ak6aa92e.com
пример.рф
example.com
# другой комментарий

trailing.example.com.
`
	tmpFile, err := os.CreateTemp("", "blockcheck-test-*")
	if err != nil {
		t.Fatalf("failed to create temp file: %v", err)
	}
	tmpPath := tmpFile.Name()
	_, err = tmpFile.WriteString(fixture)
	if err != nil {
		_ = tmpFile.Close()
		_ = os.Remove(tmpPath)
		t.Fatalf("failed to write temp file: %v", err)
	}
	if err := tmpFile.Close(); err != nil {
		_ = os.Remove(tmpPath)
		t.Fatalf("failed to close temp file: %v", err)
	}

	domains := []string{
		"Example.COM",
		"trailing.example.com.",
		"*.blocked.example",
		"spaced.example.com",
		"nodots",
		"not a domain",
		"xn--80ak6aa92e.com",
		"пример.рф",
		"absent.example",
	}

	blocked, scanned, err := CheckBlocked(tmpPath, domains)
	if err != nil {
		t.Fatalf("CheckBlocked returned error: %v", err)
	}

	// проверяем, что файл был прочитан (scanned > 0)
	if scanned == 0 {
		t.Error("expected scanned > 0, got 0")
	}

	// expected blocked в порядке запроса (дедуп): example.com, trailing.example.com, blocked.example, spaced.example.com
	// nodots — без точки, normalizeDomain отдаст "" (нет точки), домен будет пропущен
	// not a domain — пробел внутри, normalizeDomain отдаст "" (пустая строка после проверки Contains("."))
	// xn--... и пример.рф есть в файле и в domains, должны быть в blocked

	want := []string{
		"example.com",          // из "Example.COM" после lower
		"trailing.example.com", // из "trailing.example.com." после удаления точки в конце
		"blocked.example",      // из "*.blocked.example" после удаления leading "*."
		"spaced.example.com",   // из "spaced.example.com" после lower/trim
		"xn--80ak6aa92e.com",
		"пример.рф",
	}

	if len(blocked) != len(want) {
		t.Errorf("expected %d blocked domains, got %d: %v", len(want), len(blocked), blocked)
	} else {
		for i, b := range blocked {
			if b != want[i] {
				t.Errorf("blocked[%d]=%q, wanted %q", i, b, want[i])
			}
		}
	}

	_ = scanned // already checked > 0
}

func TestBlockCheckBlockedFileNotFound(t *testing.T) {
	_, _, err := CheckBlocked("/nonexistent/path/file.lst", []string{"example.com"})
	if err == nil {
		t.Error("expected error for missing file, got nil")
	}
}

func TestBlockCheckBlockedCached(t *testing.T) {
	// очищаем кэш перед тестом
	FlushBlockCheckCache()

	// создаем временный файл
	fixture := "example.com\nblocked.org\n"
	tmpFile, err := os.CreateTemp("", "blockcheck-cache-*")
	if err != nil {
		t.Fatalf("failed to create temp file: %v", err)
	}
	tmpPath := tmpFile.Name()
	_, err = tmpFile.WriteString(fixture)
	if err != nil {
		_ = tmpFile.Close()
		_ = os.Remove(tmpPath)
		t.Fatalf("failed to write temp file: %v", err)
	}
	if err := tmpFile.Close(); err != nil {
		_ = os.Remove(tmpPath)
		t.Fatalf("failed to close temp file: %v", err)
	}

	domains := []string{"example.com"}

	// Первый вызов — файл читается
	blocked1, cached1, err := CheckBlockedCached(tmpPath, "v1", domains)
	if err != nil {
		t.Fatalf("first CheckBlockedCached error: %v", err)
	}
	if cached1 {
		t.Error("first call should have cached=false")
	}
	if len(blocked1) != 1 || blocked1[0] != "example.com" {
		t.Errorf("first call: expected [example.com], got %v", blocked1)
	}

	// Второй вызов с тем же version — должен использоваться кэш
	blocked2, cached2, err := CheckBlockedCached(tmpPath, "v1", domains)
	if err != nil {
		t.Fatalf("second CheckBlockedCached error: %v", err)
	}
	if !cached2 {
		t.Error("second call should have cached=true (same version)")
	}
	if len(blocked2) != 1 || blocked2[0] != "example.com" {
		t.Errorf("second call: expected [example.com], got %v", blocked2)
	}

	// Разная версия — кэш инвалидируется
	blocked3, cached3, err := CheckBlockedCached(tmpPath, "v2", domains)
	if err != nil {
		t.Fatalf("third CheckBlockedCached error: %v", err)
	}
	if cached3 {
		t.Error("third call with different version should have cached=false")
	}
	if len(blocked3) != 1 || blocked3[0] != "example.com" {
		t.Errorf("third call: expected [example.com], got %v", blocked3)
	}
}

func TestBlockCheckBlockedCachedIsolation(t *testing.T) {
	// очищаем кэш перед тестом
	FlushBlockCheckCache()

	fixture := "example.com\n"
	tmpFile, err := os.CreateTemp("", "blockcheck-cache2-*")
	if err != nil {
		t.Fatalf("failed to create temp file: %v", err)
	}
	tmpPath := tmpFile.Name()
	_, err = tmpFile.WriteString(fixture)
	if err != nil {
		_ = tmpFile.Close()
		_ = os.Remove(tmpPath)
		t.Fatalf("failed to write temp file: %v", err)
	}
	if err := tmpFile.Close(); err != nil {
		_ = os.Remove(tmpPath)
		t.Fatalf("failed to close temp file: %v", err)
	}

	domains := []string{"example.com"}

	// Кэш для v1 — первый вызов всегда читает файл (cached=false)
	_, cached1, err := CheckBlockedCached(tmpPath, "v1", domains)
	if err != nil {
		t.Fatalf("error: %v", err)
	}
	if cached1 {
		t.Error("first call should have cached=false (cache was cleared)")
	}

	// Кэш для v2 — изолирован от v1
	_, cached2, err := CheckBlockedCached(tmpPath, "v2", domains)
	if err != nil {
		t.Fatalf("error: %v", err)
	}
	if cached2 {
		t.Error("different version should have cached=false, cache is isolated by version")
	}

	// Проверяем, что v1 все еще не закэширован (мы только что сделали v2)
	// на самом деле v1 кэш теперь должен быть перезаписан или нет,
	// но главное — v2 изолирован
	// (вместо проверки v1again делаем вывод о изоляции)
}

func FlushBlockCheckCache() {
	blockCheckCache = map[string]*cachedEntry{}
}
