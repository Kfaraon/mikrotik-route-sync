package mikrotik

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/Kfaraon/mikrotik-route-sync/internal/config"
)

// Transaction представляет транзакцию применения изменений маршрутов
type Transaction struct {
	client    *Client
	cfg       *config.Config
	service   string
	log       *slog.Logger
	added     []Route
	deleted   []Route
	startedAt time.Time
	committed bool
}

// NewTransaction создает новую транзакцию для сервиса
func NewTransaction(client *Client, cfg *config.Config, service string, log *slog.Logger) *Transaction {
	return &Transaction{
		client:    client,
		cfg:       cfg,
		service:   service,
		log:       log.With("service", service, "component", "transaction"),
		startedAt: time.Now(),
	}
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

		// ИСПРАВЛЕНО: используем DeleteRoute вместо RemoveRoute
		if err := t.client.DeleteRoute(ctx, r.ID); err != nil {
			return fmt.Errorf("remove route %s (id=%s): %w", r.DstAddress, r.ID, err)
		}
		t.deleted = append(t.deleted, r)
	}
	return nil
}

func (t *Transaction) rollback(ctx context.Context) {
	if t.committed {
		return
	}

	t.log.Warn("выполняю rollback", "added_to_remove", len(t.added), "deleted_to_restore", len(t.deleted))

	rollbackFailed := false

	if len(t.added) > 0 {
		// ИСПРАВЛЕНО: используем ListServiceRoutes вместо ListRoutes
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
					// ИСПРАВЛЕНО: используем DeleteRoute вместо RemoveRoute
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
		deleted.ID = ""
		if err := t.client.AddRoute(ctx, deleted); err != nil {
			t.log.Error("rollback: не удалось восстановить удаленный маршрут",
				"dst", deleted.DstAddress, "err", err)
			rollbackFailed = true
		}
	}

	if rollbackFailed {
		t.log.Error("rollback частично неуспешен — сервис в состоянии degraded")
	} else {
		t.log.Info("rollback успешно завершен")
	}
}

func (t *Transaction) Rollback(ctx context.Context) {
	t.rollback(ctx)
}

func (t *Transaction) Stats() (added, deleted int) {
	return len(t.added), len(t.deleted)
}
