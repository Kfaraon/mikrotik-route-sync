package mikrotik

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/Kfaraon/mikrotik-route-sync/internal/config"
	"github.com/Kfaraon/mikrotik-route-sync/internal/storage"
)

// Transaction представляет транзакцию применения изменений маршрутов
type Transaction struct {
	client     *Client
	cfg        *config.Config
	cache      *storage.Cache
	service    string
	snapshotID string
	log        *slog.Logger
	added      []Route
	deleted    []Route
	startedAt  time.Time
	committed  bool
}

// NewTransaction создает новую транзакцию для сервиса
func NewTransaction(client *Client, cfg *config.Config, cache *storage.Cache, service string, log *slog.Logger) *Transaction {
	return &Transaction{
		client:    client,
		cfg:       cfg,
		cache:     cache,
		service:   service,
		log:       log.With("service", service, "component", "transaction"),
		startedAt: time.Now(),
	}
}

// SetSnapshotID устанавливает ID snapshot для возможного отката
func (t *Transaction) SetSnapshotID(snapshotID string) {
	t.snapshotID = snapshotID
}

// Apply применяет изменения: сначала добавляет новые маршруты, потом удаляет старые
func (t *Transaction) Apply(ctx context.Context, toAdd, toDelete []Route) error {
	t.log.Info("начинаю транзакцию", "add", len(toAdd), "delete", len(toDelete))

	if len(toAdd) > 0 {
		if err := t.applyAdd(ctx, toAdd); err != nil {
			t.log.Error("ошибка добавления маршрутов", "err", err)
			t.rollback(ctx)
			return fmt.Errorf("add routes: %w", err)
		}
		t.log.Info("добавлено маршрутов", "count", len(t.added))
	}

	if len(toDelete) > 0 {
		if err := t.applyDelete(ctx, toDelete); err != nil {
			t.log.Error("ошибка удаления маршрутов", "err", err)
			t.rollback(ctx)
			return fmt.Errorf("delete routes: %w", err)
		}
		t.log.Info("удалено маршрутов", "count", len(t.deleted))
	}

	t.committed = true
	t.log.Info("транзакция завершена успешно", "duration", time.Since(t.startedAt))
	return nil
}

func (t *Transaction) applyAdd(ctx context.Context, routes []Route) error {
	for _, r := range routes {
		if r.Gateway == "" {
			r.Gateway = t.cfg.MikroTik.Gateway
		}
		if r.RoutingTable == "" {
			r.RoutingTable = t.cfg.MikroTik.RoutingTable
		}
		if r.Distance == "" {
			r.Distance = fmt.Sprintf("%d", t.cfg.MikroTik.Distance)
		}
		if r.Comment == "" {
			r.Comment = fmt.Sprintf("%s:%s", t.cfg.MikroTik.CommentPrefix, t.service)
		}

		if err := t.client.AddRoute(ctx, r); err != nil {
			return fmt.Errorf("add route %s: %w", r.DstAddress, err)
		}
		t.added = append(t.added, r)
	}
	return nil
}

func (t *Transaction) applyDelete(ctx context.Context, routes []Route) error {
	for _, r := range routes {
		if r.ID == "" {
			t.log.Warn("маршрут без ID, пропускаю удаление", "dst", r.DstAddress)
			continue
		}

		if err := t.client.DeleteRoute(ctx, r.ID); err != nil {
			return fmt.Errorf("remove route %s (id=%s): %w", r.DstAddress, r.ID, err)
		}
		t.deleted = append(t.deleted, r)
	}
	return nil
}

// rollback выполняет откат транзакции
// Сначала пытается использовать snapshot, затем fallback на локальное состояние
func (t *Transaction) rollback(ctx context.Context) {
	if t.committed {
		return
	}

	t.log.Warn("выполняю rollback",
		"added_to_remove", len(t.added),
		"deleted_to_restore", len(t.deleted),
		"snapshot_id", t.snapshotID)

	// Приоритет 1: Если есть snapshot ID, восстанавливаем из snapshot
	if t.snapshotID != "" {
		t.log.Info("использую snapshot для rollback", "snapshot_id", t.snapshotID)
		if err := t.rollbackFromSnapshot(ctx); err != nil {
			t.log.Error("rollback из snapshot не удался, использую локальное состояние", "err", err)
			t.rollbackFromLocalState(ctx)
		} else {
			t.log.Info("rollback из snapshot успешно завершен")
			return
		}
	} else {
		// Приоритет 2: Используем локальное состояние
		t.rollbackFromLocalState(ctx)
	}
}

// rollbackFromSnapshot восстанавливает сервис из snapshot
func (t *Transaction) rollbackFromSnapshot(ctx context.Context) error {
	if t.cache == nil {
		return fmt.Errorf("cache не инициализирован")
	}

	// Загружаем snapshot из хранилища
	var snapshotRoutes []Route
	if err := t.cache.GetSnapshot(t.service, t.snapshotID, &snapshotRoutes); err != nil {
		return fmt.Errorf("load snapshot: %w", err)
	}

	t.log.Info("загружен snapshot для rollback", "route_count", len(snapshotRoutes))

	// Шаг 1: Удаляем все текущие маршруты сервиса
	removed, err := t.cleanupService(ctx)
	if err != nil {
		return fmt.Errorf("cleanup service: %w", err)
	}
	t.log.Info("текущие маршруты удалены", "removed", removed)

	// Шаг 2: Восстанавливаем маршруты из snapshot
	restored := 0
	errors := 0

	for _, route := range snapshotRoutes {
		// Устанавливаем комментарий, если он пустой
		if route.Comment == "" {
			route.Comment = fmt.Sprintf("%s:%s", t.cfg.MikroTik.CommentPrefix, t.service)
		}

		// Устанавливаем значения по умолчанию
		if route.Gateway == "" {
			route.Gateway = t.cfg.MikroTik.Gateway
		}
		if route.RoutingTable == "" {
			route.RoutingTable = t.cfg.MikroTik.RoutingTable
		}
		if route.Distance == "" {
			route.Distance = fmt.Sprintf("%d", t.cfg.MikroTik.Distance)
		}

		if _, err := t.client.AddRoute(ctx, route); err != nil {
			errors++
			t.log.Error("не удалось восстановить маршрут из snapshot",
				"dst", route.DstAddress,
				"err", err)
			continue
		}
		restored++
	}

	if errors > 0 {
		return fmt.Errorf("partial restore: restored %d/%d routes (%d failed)",
			restored, len(snapshotRoutes), errors)
	}

	t.log.Info("успешное восстановление из snapshot",
		"restored", restored,
		"duration", time.Since(t.startedAt))

	return nil
}

// rollbackFromLocalState использует локальное состояние транзакции для отката
func (t *Transaction) rollbackFromLocalState(ctx context.Context) {
	rollbackFailed := false

	if len(t.added) > 0 {
		// Получаем актуальный список маршрутов
		currentRoutes, err := t.client.ListServiceRoutes(ctx, t.service)
		if err != nil {
			t.log.Error("rollback: не удалось получить список маршрутов", "err", err)
			rollbackFailed = true
		} else {
			routeMap := make(map[string]Route)
			for _, r := range currentRoutes {
				routeMap[r.DstAddress] = r
			}

			for _, added := range t.added {
				if found, ok := routeMap[added.DstAddress]; ok {
					if err := t.client.DeleteRoute(ctx, found.ID); err != nil {
						t.log.Error("rollback: не удалось удалить добавленный маршрут",
							"id", found.ID, "dst", added.DstAddress, "err", err)
						rollbackFailed = true
					}
				}
			}
		}
	}

	for _, deleted := range t.deleted {
		deleted.ID = "" // Очищаем ID, так как он устарел
		if err := t.client.AddRoute(ctx, deleted); err != nil {
			t.log.Error("rollback: не удалось восстановить удаленный маршрут",
				"dst", deleted.DstAddress, "err", err)
			rollbackFailed = true
		}
	}

	if rollbackFailed {
		t.log.Error("rollback частично неуспешен — сервис в состоянии degraded")
	} else {
		t.log.Info("rollback из локального состояния успешно завершен")
	}
}

// cleanupService удаляет все маршруты указанного сервиса
func (t *Transaction) cleanupService(ctx context.Context) (int, error) {
	routes, err := t.client.ListServiceRoutes(ctx, t.service)
	if err != nil {
		return 0, fmt.Errorf("list routes: %w", err)
	}

	removed := 0
	errors := 0

	for _, route := range routes {
		if route.ID == "" {
			t.log.Warn("маршрут без ID, пропускаю удаление", "dst", route.DstAddress)
			continue
		}

		if err := t.client.DeleteRoute(ctx, route.ID); err != nil {
			errors++
			t.log.Error("не удалось удалить маршрут",
				"id", route.ID,
				"dst", route.DstAddress,
				"err", err)
			continue
		}
		removed++
	}

	if errors > 0 {
		return removed, fmt.Errorf("deleted %d/%d routes (%d failed)",
			removed, len(routes), errors)
	}

	return removed, nil
}

// Rollback публичный метод для ручного отката
func (t *Transaction) Rollback(ctx context.Context) {
	t.rollback(ctx)
}

// Stats возвращает статистику транзакции
func (t *Transaction) Stats() (added, deleted int) {
	return len(t.added), len(t.deleted)
}
