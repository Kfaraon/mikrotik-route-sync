package core

// Контроль покрытия: входят ли текущие A-записи доменов сервиса в собранный
// набор префиксов. Если нет — сервис через VPN работать не будет (или будет
// частично), о чём сигнализируем WARN-логом и полем в результате/отчёте.
// Проверка best-effort: сбой резолва не валит сбор, покрытие считается полным.

import (
	"context"
	"net/netip"
)

// CoverageReport — покрытие текущих A-записей собранным набором префиксов.
type CoverageReport struct {
	Checked   int      `json:"checked"`
	Covered   int      `json:"covered"`
	Uncovered []string `json:"uncovered,omitempty"`
}

// Complete — все проверенные адреса входят в набор (или проверять нечего:
// сервис без доменов).
func (r *CoverageReport) Complete() bool {
	return r == nil || len(r.Uncovered) == 0
}

// checkCoverage — чистая логика сверки: резолв доменов через resolve,
// сверка каждого IPv4-адреса с набором префиксов. IPv6 не участвует
// (проект работает только с IPv4).
func checkCoverage(ctx context.Context, resolve func(context.Context, string) ([]string, error), domains []string, prefixes []netip.Prefix) *CoverageReport {
	rep := &CoverageReport{}
	seen := map[string]bool{}
	for _, d := range domains {
		if ctx.Err() != nil {
			break
		}
		ips, err := resolve(ctx, d)
		if err != nil || len(ips) == 0 {
			continue
		}
		for _, s := range ips {
			ip, perr := netip.ParseAddr(s)
			if perr != nil || !ip.Is4() || seen[ip.String()] {
				continue
			}
			seen[ip.String()] = true
			rep.Checked++
			covered := false
			for _, p := range prefixes {
				if p.Contains(ip) {
					covered = true
					break
				}
			}
			if covered {
				rep.Covered++
			} else {
				rep.Uncovered = append(rep.Uncovered, ip.String())
			}
		}
	}
	return rep
}

// checkServiceCoverage — покрытие для сервиса: домены через serviceDomains,
// резолв через резолвер синкера.
func (s *Syncer) checkServiceCoverage(ctx context.Context, name string, domains []string, prefixes []netip.Prefix) *CoverageReport {
	if len(domains) == 0 {
		return &CoverageReport{}
	}
	return checkCoverage(ctx, func(ctx context.Context, d string) ([]string, error) {
		return s.resolver().ResolveDomain(ctx, d)
	}, domains, prefixes)
}
