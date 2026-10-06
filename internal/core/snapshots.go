package core

// Снапшоты набора маршрутов сервиса.

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/Kfaraon/mikrotik-route-sync/internal/addresslist"
)

type SnapshotInfo struct {
	ID        string    `json:"id"`
	Service   string    `json:"service"`
	CreatedAt time.Time `json:"created_at"`
	Count     int       `json:"count"`
}

func (s *Syncer) createSnapshot(ctx context.Context, name string, entries []addresslist.Entry) string {
	if !s.cfg.Snapshots.Enabled {
		return ""
	}

	addresses := make([]string, 0, len(entries))
	skipped := 0
	for _, e := range entries {
		norm, err := addresslist.NormalizeAddress(e.Address)
		if err != nil {
			s.log.Error("invalid address in snapshot, skipping",
				"address", e.Address, "service", name, "err", err)
			skipped++
			continue
		}
		addresses = append(addresses, norm)
	}
	if skipped > 0 {
		s.log.Warn("some entries skipped in snapshot", "service", name, "skipped", skipped, "total", len(entries))
	}

	id, err := s.CreateSnapshot(ctx, name, addresses)
	if err != nil {
		s.log.Error("failed to create snapshot", "service", name, "err", err)
		return ""
	}
	return id
}

func (s *Syncer) CreateSnapshot(ctx context.Context, service string, addresses []string) (string, error) {
	if !s.cfg.Snapshots.Enabled {
		return "", fmt.Errorf("snapshots are disabled")
	}
	id, err := s.cache.CreateSnapshot(service, addresses)
	if err != nil {
		return "", fmt.Errorf("save snapshot: %w", err)
	}
	s.log.Info("created snapshot", "service", service, "id", id, "addresses", len(addresses))
	return id, nil
}

func (s *Syncer) GetSnapshot(ctx context.Context, service, id string, entries *[]addresslist.Entry) error {
	var addresses []string
	if err := s.cache.GetSnapshot(service, id, &addresses); err != nil {
		return fmt.Errorf("load snapshot: %w", err)
	}

	list := s.cfg.Firewall.AddressList
	comment := addresslist.CommentFor(s.cfg.Firewall.CommentPrefix, service)
	out := make([]addresslist.Entry, 0, len(addresses))
	for _, a := range addresses {
		out = append(out, addresslist.Entry{Address: a, List: list, Comment: comment, Disabled: "false"})
	}
	*entries = out
	return nil
}

func (s *Syncer) ListSnapshots(ctx context.Context, service string) ([]SnapshotInfo, error) {
	infos, err := s.cache.ListSnapshots(service)
	if err != nil {
		return nil, fmt.Errorf("list snapshots: %w", err)
	}
	result := make([]SnapshotInfo, 0, len(infos))
	for _, info := range infos {
		result = append(result, SnapshotInfo{
			ID:        info.ID,
			Service:   service,
			CreatedAt: info.CreatedAt,
			Count:     info.Count,
		})
	}
	return result, nil
}

func (s *Syncer) DeleteSnapshot(ctx context.Context, service, id string) error {
	return s.cache.DeleteSnapshot(service, id)
}

func (s *Syncer) CleanupSnapshots(ctx context.Context, service string, ttl time.Duration) (int, error) {
	ids, err := s.cache.ListSnapshots(service)
	if err != nil {
		return 0, fmt.Errorf("list snapshots: %w", err)
	}

	deleted := 0
	cutoff := time.Now().Add(-ttl)
	for _, info := range ids {
		if info.CreatedAt.Before(cutoff) {
			if err := s.cache.DeleteSnapshot(service, info.ID); err != nil {
				s.log.Warn("failed to delete expired snapshot", "service", service, "id", info.ID, "err", err)
				continue
			}
			deleted++
		}
	}

	remaining, err := s.cache.ListSnapshots(service)
	if err != nil {
		return deleted, nil
	}
	maxCount := s.cfg.Snapshots.MaxCount
	if maxCount > 0 && len(remaining) > maxCount {
		sort.Slice(remaining, func(i, j int) bool {
			return remaining[i].CreatedAt.Before(remaining[j].CreatedAt)
		})
		toDelete := len(remaining) - maxCount
		for i := 0; i < toDelete && i < len(remaining); i++ {
			if err := s.cache.DeleteSnapshot(service, remaining[i].ID); err != nil {
				s.log.Warn("failed to delete excess snapshot", "service", service, "id", remaining[i].ID, "err", err)
				continue
			}
			deleted++
		}
	}

	s.log.Info("snapshot cleanup completed", "service", service, "deleted", deleted, "ttl", ttl, "max_count", maxCount)
	return deleted, nil
}

// ============================================================================
// Управление сервисами
// ============================================================================
