package mikrotik

import (
	"context"
	"fmt"
	"time"
)

type Backup struct {
	Service   string
	Routes    []Route
	CreatedAt time.Time
}

// Tx — транзакция по одному сервису.
// Гарантия: при любой ошибке состояние AUTO:<service> восстанавливается.
type Tx struct {
	client  *Client
	backups map[string]*Backup
}

func (c *Client) BeginTx() *Tx {
	return &Tx{client: c, backups: map[string]*Backup{}}
}

func (t *Tx) Apply(ctx context.Context, service string, toAdd, toRemove []string) error {
	comment := "AUTO:" + service

	// 1. Снимок.
	current, err := t.client.ListRoutes(ctx, comment)
	if err != nil {
		return fmt.Errorf("snapshot %s: %w", comment, err)
	}
	t.backups[service] = &Backup{Service: service, Routes: current, CreatedAt: time.Now()}

	// 2. Удаление.
	for _, r := range toRemove {
		if err := t.client.DeleteRoute(ctx, r); err != nil {
			t.rollback(ctx, service)
			return fmt.Errorf("delete %s: %w", r, err)
		}
	}

	// 3. Добавление.
	for _, cidr := range toAdd {
		if err := t.client.AddRoute(ctx, cidr, comment); err != nil {
			t.rollback(ctx, service)
			return fmt.Errorf("add %s: %w", cidr, err)
		}
	}
	return nil
}

func (t *Tx) rollback(ctx context.Context, service string) {
	b, ok := t.backups[service]
	if !ok {
		return
	}
	comment := "AUTO:" + service
	// Удаляем всё, что нагородили.
	_ = t.client.DeleteAllRoutes(ctx, comment)
	// Восстанавливаем из снимка.
	for _, r := range b.Routes {
		_ = t.client.AddRoute(ctx, r.DstAddress, comment)
	}
}
