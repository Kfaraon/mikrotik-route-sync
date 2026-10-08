package presets

// index.go — инвертированный индекс «домен → пресеты» для автоподсказки
// при добавлении сервиса: по введённому домену подсказываем, в какие
// пресеты его можно добавить.
//
// Свой парсинг здесь не пишется: берётся тот же parser, что у
// ParseUpstream. Поэтому include: раскрывается рекурсивно с защитой
// от циклов, а @ads/@telemetry, keyword:/regexp: и безточечные
// TLD-записи (youtube, telegram — не A-хосты) отбрасываются
// автоматически и не дублируются в коде.

import (
	"sort"
	"strings"
)

// Index — обратный индекс «домен → слаги пресетов».
//
// Ключи — домены в нижнем регистре без финальной точки (как их
// возвращает sanitizeDomain). Значения — слаги по возрастанию, без
// дублей: файлы обходятся в отсортированном порядке, и каждый слаг
// под доменом добавляется не более одного раза.
//
// На весь снимок v2fly приходится ~110k ключей — обычный
// map[string][]string, без уплотнения памяти и кэшей.
type Index struct {
	byDomain map[string][]string
}

// BuildIndex строит индекс по снимку data/* из v2fly/domain-list-community.
//
// files — та же карта «слаг (имя файла) → содержимое», что у ParseUpstream.
// Обрабатываются все файлы карты, а не только каталог: подсказка должна
// сработать на домен из любого списка. Слаг берётся из ключа файла,
// поэтому домен, разрешённый в нескольких списках (собственно и через
// include:), получает несколько слагов — без дублей.
//
// Возвращает непустой указатель и на пустом входе: Match на нём
// просто вернёт nil.
func BuildIndex(files map[string]string) *Index {
	ix := &Index{byDomain: make(map[string][]string)}
	if len(files) == 0 {
		return ix
	}

	// Слаги по возрастанию: порядок слагов внутри домена детерминирован
	// и не зависит от рандомизации итерации по map.
	slugs := make([]string, 0, len(files))
	for slug := range files {
		slugs = append(slugs, slug)
	}
	sort.Strings(slugs)

	// Счётчики Stats здесь никуда не идут — parser требует непустой
	// указатель, а статистика конвертации индексу не нужна.
	p := &parser{files: files, stats: &Stats{}}
	for _, slug := range slugs {
		p.reset() // свой stack/done на каждый файл: циклы и дедуп include — своя защита
		for _, domain := range p.resolve(slug) {
			ix.add(domain, slug)
		}
	}
	return ix
}

// add кладёт слаг под домен, пропуская дубль (порядок первого
// появления сохраняется).
func (ix *Index) add(domain, slug string) {
	if contains(ix.byDomain[domain], slug) {
		return
	}
	ix.byDomain[domain] = append(ix.byDomain[domain], slug)
}

// Match возвращает слаги пресетов, которым подходит домен.
//
// Нормализация — общая (normalizeDomain): lower, trim пробелов,
// без точки в конце. Дальше:
//  1. точное совпадение ключа;
//  2. apex: срезается первая метка, проверяется максимум два уровня
//     вверх, пока в кандидате остаётся точка (TLD-кандидат ключом не
//     бывает — безточечные записи в индекс не попадают);
//  3. результат — точные первыми, затем apex, без дублей слагов.
//
// nil — если ничего не нашлось или вход пуст.
func (ix *Index) Match(domain string) []string {
	if ix == nil {
		return nil
	}
	d := normalizeDomain(domain)
	if d == "" {
		return nil
	}

	// Копия: caller не должен держать ссылку на внутренний срез.
	out := append([]string(nil), ix.byDomain[d]...)

	// Apex-уровни: максимум два среза первой метки.
	cur := d
	for i := 0; i < 2; i++ {
		dot := strings.IndexByte(cur, '.')
		if dot < 0 {
			break
		}
		cur = cur[dot+1:]
		if !strings.Contains(cur, ".") {
			break // дошли до TLD — такого ключа в индексе не бывает
		}
		for _, slug := range ix.byDomain[cur] {
			if !contains(out, slug) {
				out = append(out, slug)
			}
		}
	}

	if len(out) == 0 {
		return nil
	}
	return out
}
