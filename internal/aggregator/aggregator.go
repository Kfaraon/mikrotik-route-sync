package aggregator

import (
	"fmt"
	"math/big"
	"net/netip"
	"sort"
	"strings"

	"github.com/Kfaraon/mikrotik-route-sync/internal/config"
)

// Значения по умолчанию для минимальных масок (используются если не передан SafetyConfig)
const defaultMinV4Bits = 9
const defaultMinV6Bits = 32

// Validate проверяет префикс на валидность и отсутствие пересечений с зарезервированными диапазонами.
// Принимает необязательный параметр safety для переопределения минимальных масок из конфига.
func Validate(p netip.Prefix, safety *config.SafetyConfig) error {
	if !p.IsValid() {
		return fmt.Errorf("invalid prefix")
	}
	p = p.Masked()
	
	// Используем значения из конфига или дефолтные
	minV4 := defaultMinV4Bits
	minV6 := defaultMinV6Bits
	if safety != nil {
		if safety.MinPrefixV4 > 0 {
			minV4 = safety.MinPrefixV4
		}
		if safety.MinPrefixV6 > 0 {
			minV6 = safety.MinPrefixV6
		}
	}
	
	if p.Addr().Is4() {
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
	if p.Bits() < minV6 {
		return fmt.Errorf("prefix %s too wide (min /%d)", p, minV6)
	}
	for _, r := range reservedV6 {
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
// Выполняет парсинг, валидацию, фильтрацию и удаление дубликатов.
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
	
	// Фильтруем и валидируем (используем nil для safety, т.к. NormalizeAll вызывается без контекста конфига)
	valid, validationErrors := FilterValid(prefixes, nil)
	if len(validationErrors) > 0 {
		return nil, fmt.Errorf("validation errors: %v", validationErrors)
	}
	
	// Удаляем дубликаты
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

// Aggregate агрегирует список префиксов: удаляет содержащиеся, сортирует и объединяет смежные.
func Aggregate(in []netip.Prefix) ([]netip.Prefix, error) {
	if len(in) == 0 {
		return nil, nil
	}
	
	// Валидируем
	valid, errs := FilterValid(in, nil)
	if len(errs) > 0 {
		return nil, fmt.Errorf("validation errors: %v", errs)
	}
	
	// Удаляем содержащиеся
	filtered := RemoveContained(valid)
	
	// Разделяем IPv4 и IPv6
	var v4, v6 []netip.Prefix
	for _, p := range filtered {
		if p.Addr().Is4() {
			v4 = append(v4, p)
		} else {
			v6 = append(v6, p)
		}
	}
	
	// Сортируем
	sort.Slice(v4, func(i, j int) bool {
		if v4[i].Bits() != v4[j].Bits() {
			return v4[i].Bits() < v4[j].Bits()
		}
		return v4[i].Addr().Less(v4[j].Addr())
	})
	sort.Slice(v6, func(i, j int) bool {
		if v6[i].Bits() != v6[j].Bits() {
			return v6[i].Bits() < v6[j].Bits()
		}
		return v6[i].Addr().Less(v6[j].Addr())
	})
	
	// Объединяем смежные
	v4 = mergeAdjacent(v4)
	v6 = mergeAdjacent(v6)
	
	// Объединяем результаты
	result := append(v4, v6...)
	
	return result, nil
}

// AggregateStrings агрегирует список строк-префиксов.
func AggregateStrings(raw []string) ([]netip.Prefix, error) {
	normalized, err := NormalizeAll(raw)
	if err != nil {
		return nil, fmt.Errorf("normalize: %w", err)
	}
	
	return Aggregate(normalized)
}

// countAddresses подсчитывает количество IP-адресов в списке префиксов.
func countAddresses(prefixes []netip.Prefix) *big.Int {
	return sumAddresses(prefixes)
}

// coverageInvariant проверяет инвариант покрытия: сумма адресов должна быть одинаковой.
func coverageInvariant(before, after []netip.Prefix) bool {
	sumBefore := sumAddresses(before)
	sumAfter := sumAddresses(after)
	return sumBefore.Cmp(sumAfter) == 0
}
