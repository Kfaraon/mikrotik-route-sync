package core

// Запросы к firewall address-list: diff, аудит, чистка, списки записей.

import (
	"context"
	"fmt"
	"log/slog"
	"net/netip"
	"strings"

	"github.com/Kfaraon/mikrotik-route-sync/internal/addresslist"
	"github.com/Kfaraon/mikrotik-route-sync/internal/aggregator"
	"github.com/Kfaraon/mikrotik-route-sync/internal/validator"
)

type OrphanEntry struct {
	Addresslist addresslist.Entry `json:"entry"`
	Service     string            `json:"comment_service"`
}

func (s *Syncer) DiffService(ctx context.Context, name string) (*addresslist.Diff, error) {
	// Снапшот конфига: дальше — сеть и расчёты без блокировок.
	safety := s.cfg.SnapshotSafety()
	s.cfg.RLock()
	list := s.cfg.Firewall.AddressList
	comment := addresslist.CommentFor(s.cfg.Firewall.CommentPrefix, name)
	ov := s.cfg.Overrides[name]
	maxASN := s.cfg.Safety.MaxASNPrefixes
	s.cfg.RUnlock()

	all, err := s.mt().ListEntries(ctx, list)
	if err != nil {
		return nil, fmt.Errorf("list entries: %w", err)
	}
	existing := addresslist.FilterManaged(all, list, comment)
	foreign := addresslist.ForeignEntries(all, list, comment)

	raw, _, err := s.collectOrStub(ctx, name, ov, maxASN, nil)
	if err != nil {
		return nil, fmt.Errorf("collect: %w", err)
	}

	v := validator.Validator{Safety: safety}
	prefixes, rej, err := v.ValidateWithStats(prefixesToStrings(raw), ov)
	if err != nil {
		return nil, fmt.Errorf("validate: %w", err)
	}
	if rej.Total() > 0 {
		s.log.Warn("validator rejected some prefixes",
			"ipv6", rej.IPv6, "blocked", rej.Blocked, "too_broad", rej.TooBroad,
			"host_routes", rej.HostRoute, "excluded", rej.Excluded, "not_included", rej.NotIncluded)
	}
	if len(prefixes) == 0 {
		return nil, fmt.Errorf("no valid prefixes after validation")
	}

	agg, err := aggregator.Aggregate(prefixes)
	if err != nil {
		agg = prefixes
	}

	desired := make([]string, 0, len(agg))
	for _, p := range agg {
		desired = append(desired, p.String())
	}
	desired = applyForeignExclusions(desired, foreign, s.log)
	if len(desired) == 0 {
		// Весь набор уже покрыт записями списка — изменений нет
		// (та же семантика, что и в SyncService: «уже покрыто», не ошибка).
		return &addresslist.Diff{}, nil
	}
	return addresslist.ComputeDiff(name, list, comment, desired, existing)
}

// applyForeignExclusions вычитает из желаемого набора уже существующие чужие
// записи списка (manual, другие сервисы, dynamic), чтобы не создавать
// пересекающиеся адреса, и ре-агрегирует результат. Без чужих записей или
// пересечений возвращает набор без изменений.

func applyForeignExclusions(desired []string, foreign []addresslist.Entry, log *slog.Logger) []string {
	if len(desired) == 0 || len(foreign) == 0 {
		return desired
	}

	out, hits := addresslist.SubtractForeign(desired, foreign)
	if hits == 0 {
		return desired
	}
	log.Info("excluded overlaps with existing MikroTik entries",
		"overlapped", hits, "before", len(desired), "after", len(out))

	parsed := make([]netip.Prefix, 0, len(out))
	for _, d := range out {
		if p, err := netip.ParsePrefix(d); err == nil {
			parsed = append(parsed, p)
		}
	}
	merged, err := aggregator.Aggregate(parsed)
	if err != nil {
		return out
	}
	re := make([]string, 0, len(merged))
	for _, p := range merged {
		re = append(re, p.String())
	}
	return re
}

func (s *Syncer) ListGlobalEntries(ctx context.Context) ([]addresslist.Entry, error) {
	s.cfg.RLock()
	list := s.cfg.Firewall.AddressList
	s.cfg.RUnlock()
	return s.mt().ListEntries(ctx, list)
}

func (s *Syncer) ListServiceEntries(ctx context.Context, name string) ([]addresslist.Entry, error) {
	s.cfg.RLock()
	list := s.cfg.Firewall.AddressList
	prefix := s.cfg.Firewall.CommentPrefix
	s.cfg.RUnlock()
	return s.mt().ListServiceEntries(ctx, list, prefix, name)
}

// ServiceEntryCounts возвращает число managed-записей по каждому сервису за
// ОДИН запрос к роутеру: раньше UI делал N запросов (по одному на сервис),
// и при 3-секундном дедлайне хвост молча получал Routes=0 (O1). Записи, чей
// комментарий не AUTO:<service>, игнорируются (orphan-и не в счёт).
func (s *Syncer) ServiceEntryCounts(ctx context.Context) (map[string]int, error) {
	s.cfg.RLock()
	list := s.cfg.Firewall.AddressList
	prefix := s.cfg.Firewall.CommentPrefix
	s.cfg.RUnlock()

	entries, err := s.mt().ListEntries(ctx, list)
	if err != nil {
		return nil, err
	}
	counts := make(map[string]int, len(entries))
	for _, e := range entries {
		if e.IsDynamic() {
			continue
		}
		svc, ok := addresslist.ServiceFromComment(e.Comment, prefix)
		if !ok {
			continue
		}
		counts[svc]++
	}
	return counts, nil
}

func (s *Syncer) AuditAddressList(ctx context.Context) ([]OrphanEntry, error) {
	entries, err := s.ListGlobalEntries(ctx)
	if err != nil {
		return nil, err
	}

	s.cfg.RLock()
	prefix := s.cfg.Firewall.CommentPrefix
	active := make(map[string]bool, len(s.cfg.Services))
	for _, sv := range s.cfg.Services {
		active[sv] = true
	}
	s.cfg.RUnlock()

	var orphans []OrphanEntry
	for _, e := range entries {
		if e.IsDynamic() {
			continue
		}
		svc, ok := addresslist.ServiceFromComment(e.Comment, prefix)
		if !ok {
			continue
		}
		if active[svc] {
			continue
		}
		orphans = append(orphans, OrphanEntry{Addresslist: e, Service: svc})
	}
	return orphans, nil
}

func (s *Syncer) CleanupAutoEntries(ctx context.Context, force bool) (int, error) {
	if !force {
		return 0, fmt.Errorf("cleanup_auto_entries requires --force (orphan-записи не удаляются молча)")
	}
	orphans, err := s.AuditAddressList(ctx)
	if err != nil {
		return 0, err
	}
	deleted := 0
	for _, o := range orphans {
		if err := s.mt().DeleteEntry(ctx, o.Addresslist.ID); err != nil {
			s.log.Error("failed to delete orphan entry",
				"id", o.Addresslist.ID, "address", o.Addresslist.Address, "err", err)
			continue
		}
		deleted++
	}
	s.log.Info("orphan AUTO entries cleanup completed", "deleted", deleted, "total", len(orphans))
	return deleted, nil
}

func (s *Syncer) RemoveServiceEntries(ctx context.Context, name string) (int, error) {
	entries, err := s.ListServiceEntries(ctx, name)
	if err != nil {
		return 0, err
	}
	deleted := 0
	var failed []string
	for _, e := range entries {
		if err := s.mt().DeleteEntry(ctx, e.ID); err != nil {
			failed = append(failed, e.Address)
			s.log.Error("failed to delete entry", "service", name, "id", e.ID, "address", e.Address, "err", err)
			continue
		}
		deleted++
	}
	if len(failed) > 0 {
		return deleted, fmt.Errorf("удалено %d из %d записей, не удалены: %s",
			deleted, len(entries), strings.Join(failed, ", "))
	}
	return deleted, nil
}
