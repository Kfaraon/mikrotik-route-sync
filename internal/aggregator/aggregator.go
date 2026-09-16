package aggregator

import (
	"fmt"
	"math/big"
	"net/netip"
	"sort"
	"strings"

	"github.com/Kfaraon/mikrotik-route-sync/internal/config"
)

// Значения по умолчанию (используются если не передан SafetyConfig)
const defaultMinV4Bits = 9
const defaultMinV6Bits = 32

// ... (остальной код без изменений: RemoveContained, ContainsPrefix, NormalizeAll, siblings, etc.)

// Validate проверяет префикс на валидность и отсутствие пересечений с зарезервированными диапазонами.
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

// ... (остальные функции: Aggregate, AggregateStrings, countAddresses, coverageInvariant без изменений)
