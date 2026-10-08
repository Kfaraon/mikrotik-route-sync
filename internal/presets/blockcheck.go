package presets

import (
	"bufio"
	"os"
	"sort"
	"strings"
	"sync"
)

// CheckBlocked открывает файл и потоковым чранением находит пересечение
// доменов из domains с доменами из файла по адресу path.
//
// Нормализация строк одинакова для файла и запроса:
//   - lower case
//   - trim space (лишние пробелы по краям)
//   - remove trailing '.' (точка в конце)
//   - strip leading "*."
//
// Пустые строки и строки, начинающиеся с '#', пропускаются.
//
// Возвращает:
//   - blocked — домены из domains, которые есть в блоклисте, в порядке запроса
//     с удалением дубликатов (дедуп по нормализованному виду).
//   - scanned — число прочитанных строк из файла (включая пустые и комментарийные).
//   - err — ошибка, если файл не найден или ошибка ввода-вывода.
//     При возврате ошибки blocked может быть пустым, а scanned — частичным числом.
func CheckBlocked(path string, domains []string) (blocked []string, scanned int64, err error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, 0, err
	}
	defer func() { _ = f.Close() }()

	// Нормализуем запрошенные домены и строим карту для O(1) lookup
	// сохраняем порядок первоначального запроса
	reqNorm := make(map[string]struct{})
	reqOrder := make([]string, 0, len(domains))
	for _, d := range domains {
		n := normalizeDomain(d)
		if n == "" {
			continue
		}
		if _, exists := reqNorm[n]; !exists {
			reqNorm[n] = struct{}{}
			reqOrder = append(reqOrder, n)
		}
	}

	// blockedSet track which normalized domains from request were found in file
	blockedSet := make(map[string]struct{}, len(reqNorm))

	scanned = 0

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 1024*1024), 1024*1024)

	for scanner.Scan() {
		scanned++
		line := scanner.Text()
		trimmed := strings.TrimSpace(line)

		// Пропускаем пустые строки
		if trimmed == "" {
			continue
		}
		// Пропускаем комментарии
		if strings.HasPrefix(trimmed, "#") {
			continue
		}

		norm := normalizeDomain(trimmed)
		if norm == "" {
			continue
		}

		// Проверяем, есть ли нормализованный домен в запросе
		if _, found := reqNorm[norm]; found {
			// Добавляем в результат, если еще не было (дедуп)
			if _, already := blockedSet[norm]; !already {
				blockedSet[norm] = struct{}{}
			}
		}
	}

	if err = scanner.Err(); err != nil {
		return nil, scanned, err
	}

	// Формируем результат в порядке запроса (reqOrder), оставляя только те,
	// найденные в файле
	result := make([]string, 0, len(reqOrder))
	for _, r := range reqOrder {
		if _, found := blockedSet[r]; found {
			result = append(result, r)
		}
	}

	return result, scanned, nil
}

// normalizeDomain применяет ту же нормализацию, что и в задаче:
//   - lower case
//   - trim space (строки уже обрезаны, но на всякий случай)
//   - remove trailing '.' (точка в конце)
//   - strip leading "*."
func normalizeDomain(s string) string {
	s = strings.ToLower(s)
	// Домены пробелов не содержат: схлопываем все пробельные символы
	// ("spaced .example.com" — это "spaced.example.com" с мусором).
	s = strings.Join(strings.Fields(s), "")
	// Маркер YAML-списка: зеркало лежит пресетом ("  - example.com").
	s = strings.TrimPrefix(s, "-")
	s = strings.Trim(s, `"'`)
	// Отрезаем точку в конце
	s = strings.TrimSuffix(s, ".")
	// Отрезаем leading "*."
	if len(s) >= 2 && s[0] == '*' && s[1] == '.' {
		s = s[2:]
	}
	s = strings.TrimSpace(s)
	if s == "" || !strings.Contains(s, ".") {
		return ""
	}
	return s
}

// CheckBlockedCached потоковая проверка с пакетным кэшем.
// Ключ = version + "\x00" + sorted-lower-joined-domains.
// version дает вызывающий (дата зеркала), что позволяет инвалидировать кэш
// сменой версии. TTL нет — кэш живет до перезапуска или смены версии.
//
// cached=true означает, что файл не был прочитан (результат взят из памяти).
func CheckBlockedCached(path, version string, domains []string) (blocked []string, cached bool, err error) {
	CheckBlockedCachedMu.Lock()
	defer CheckBlockedCachedMu.Unlock()

	// создаем ключ: version + \x00 + sorted-lower-joined-domains
	normDomains := make([]string, 0, len(domains))
	for _, d := range domains {
		n := normalizeDomain(d)
		if n != "" {
			normDomains = append(normDomains, n)
		}
	}
	sort.Strings(normDomains)
	joined := strings.Join(normDomains, "\x00")
	key := version + "\x00" + joined

	// проверяем кэш
	if entry, ok := blockCheckCache[key]; ok {
		return entry.blocked, true, nil
	}

	// кэш мисс — читаем файл
	blocked, scanned, err := CheckBlocked(path, domains)
	if err != nil {
		return nil, false, err
	}

	// сохраняем в кэш
	blockCheckCache[key] = &cachedEntry{
		blocked: blocked,
		scanned: scanned,
	}

	return blocked, false, nil
}

// cachedEntry — запись в кэше.CheckBlockedCached.
type cachedEntry struct {
	blocked []string
	scanned int64
}

// CheckBlockedCachedMu is the mutex protecting the global cache for CheckBlockedCached.
var CheckBlockedCachedMu sync.Mutex

// blockCheckCache is the global cache for CheckBlockedCached.
var blockCheckCache = map[string]*cachedEntry{}
