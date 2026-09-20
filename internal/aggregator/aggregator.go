package aggregator

import (
	"fmt"
	"net/netip"
	"sort"
	"strings"

	"github.com/Kfaraon/mikrotik-route-sync/internal/config"
)

// defaultMinV4Bits — минимальная маска IPv4 по умолчанию (допустимый диапазон 8..32).
// Проект работает только с IPv4: IPv6-префиксы отклоняются валидацией.
const defaultMinV4Bits = 8

// Validate проверяет IPv4-префикс на валидность и отсутствие пересечений
// с зарезервированными диапазонами. Не-IPv4 адреса отклоняются.
// Принимает необязательный параметр safety для переопределения минимальной маски.
func Validate(p netip.Prefix, safety *config.SafetyConfig) error {
	if !p.IsValid() {
		return fmt.Errorf("invalid prefix")
	}
	if !p.Addr().Is4() {
		return fmt.Errorf("prefix %s is not IPv4 (IPv6 is not supported)", p)
	}
	p = p.Masked()

	minV4 := defaultMinV4Bits
	if safety != nil && safety.MinPrefixV4 > 0 {
		minV4 = safety.MinPrefixV4
	}

	if p.Bits() < minV4 {
		return fmt.Errorf("prefix %s too wide (min /%d)", p, minV4)
	}
	for _, r := range reservedV4 {
		if r.Overlaps(p) {
			return fmt.Errorf("prefix %s overlaps reserved %s", p, r)
		}
	}
	return nil
}

// FilterValid фильтрует список префиксов, возвращая только валидные.
// Принимает необязательный параметр safety для переопределения минимальных масок из конфига.
func FilterValid(in []netip.Prefix, safety *config.SafetyConfig) ([]netip.Prefix, []error) {
	out := make([]netip.Prefix, 0, len(in))
	var errs []error
	for _, p := range in {
		if err := Validate(p, safety); err != nil {
			errs = append(errs, err)
			continue
		}
		out = append(out, p.Masked())
	}
	return out, errs
}

// ContainsPrefix проверяет, содержит ли префикс outer префикс inner.
func ContainsPrefix(outer, inner netip.Prefix) bool {
	if !outer.IsValid() || !inner.IsValid() {
		return false
	}
	outer = outer.Masked()
	inner = inner.Masked()

	// Разные типы адресов не могут содержать друг друга
	if outer.Addr().Is4() != inner.Addr().Is4() {
		return false
	}

	// Outer должен быть шире или равен inner
	if outer.Bits() > inner.Bits() {
		return false
	}

	// Проверяем, что inner.Addr() находится в диапазоне outer
	return outer.Contains(inner.Addr())
}

// RemoveContained удаляет префиксы, которые полностью содержатся в других префиксах списка.
func RemoveContained(in []netip.Prefix) []netip.Prefix {
	if len(in) == 0 {
		return nil
	}

	// Сортируем по ширине маски (от широких к узким)
	sorted := make([]netip.Prefix, len(in))
	copy(sorted, in)
	sort.Slice(sorted, func(i, j int) bool {
		if sorted[i].Bits() != sorted[j].Bits() {
			return sorted[i].Bits() < sorted[j].Bits()
		}
		return sorted[i].Addr().Less(sorted[j].Addr())
	})

	var out []netip.Prefix
	for i, p := range sorted {
		contained := false
		for j := 0; j < i; j++ {
			if ContainsPrefix(sorted[j], p) {
				contained = true
				break
			}
		}
		if !contained {
			out = append(out, p)
		}
	}

	return out
}

// NormalizeAll нормализует список строк в список префиксов.
//
// Синтаксическая ошибка в любой строке — жёсткая ошибка (уровень 1 валидации).
// Приватные/зарезервированные и слишком широкие сети — молча отбрасываются
// (уровни 2 и 4): источники обязаны очищаться, а не ронять синхронизацию.
// Дубликаты удаляются.
func NormalizeAll(raw []string) ([]netip.Prefix, error) {
	if len(raw) == 0 {
		return nil, nil
	}

	var prefixes []netip.Prefix
	var parseErrors []string

	for _, s := range raw {
		s = strings.TrimSpace(s)
		if s == "" {
			continue
		}

		p, err := netip.ParsePrefix(s)
		if err != nil {
			parseErrors = append(parseErrors, fmt.Sprintf("invalid prefix %q: %v", s, err))
			continue
		}

		prefixes = append(prefixes, p.Masked())
	}

	if len(parseErrors) > 0 {
		return nil, fmt.Errorf("parse errors: %s", strings.Join(parseErrors, "; "))
	}

	valid, _ := FilterValid(prefixes, nil)

	seen := make(map[netip.Prefix]bool)
	var unique []netip.Prefix
	for _, p := range valid {
		if !seen[p] {
			seen[p] = true
			unique = append(unique, p)
		}
	}

	return unique, nil
}

// Aggregate выполняет безопасную агрегацию набора IPv4-префиксов.
//
// Проект работает только с IPv4: не-IPv4 префиксы отбрасываются до агрегации.
// Агрегация выполняется radix tree (объединение настоящих sibling-префиксов).
//
// После агрегации проверяются инварианты (ValidateAggregation):
// сохранность суммарного объёма адресов и полное покрытие исходного набора.
// При нарушении инварианта возвращается неагрегированный (но валидированный)
// список — это безопасный откат, а не ошибка синхронизации.
func Aggregate(in []netip.Prefix) ([]netip.Prefix, error) {
	v4 := make([]netip.Prefix, 0, len(in))
	for _, p := range in {
		if p.Addr().Is4() {
			v4 = append(v4, p)
		}
	}
	if len(v4) == 0 {
		return nil, nil
	}

	// Валидируем (IPv6 уже отброшен выше)
	valid, errs := FilterValid(v4, nil)
	if len(errs) > 0 {
		return nil, fmt.Errorf("validation errors: %v", errs)
	}

	// Удаляем содержащиеся и дубликаты
	filtered := dedupe(RemoveContained(valid))

	// Агрегация через radix tree
	merged := AggregatePrefixes(filtered)
	if err := ValidateAggregation(filtered, merged); err != nil {
		return filtered, fmt.Errorf("aggregation rolled back: %w", err)
	}

	sortPrefixes(merged)
	return merged, nil
}

// sortPrefixes сортирует префиксы по возрастанию маски, затем по адресу.
func sortPrefixes(ps []netip.Prefix) {
	sort.Slice(ps, func(i, j int) bool {
		if ps[i].Bits() != ps[j].Bits() {
			return ps[i].Bits() < ps[j].Bits()
		}
		return ps[i].Addr().Less(ps[j].Addr())
	})
}

// dedupe удаляет точные дубликаты.
func dedupe(in []netip.Prefix) []netip.Prefix {
	seen := make(map[netip.Prefix]bool, len(in))
	out := make([]netip.Prefix, 0, len(in))
	for _, p := range in {
		if !seen[p] {
			seen[p] = true
			out = append(out, p)
		}
	}
	return out
}

// AggregateStrings агрегирует список строк-префиксов.
func AggregateStrings(raw []string) ([]netip.Prefix, error) {
	normalized, err := NormalizeAll(raw)
	if err != nil {
		return nil, fmt.Errorf("normalize: %w", err)
	}

	return Aggregate(normalized)
}
