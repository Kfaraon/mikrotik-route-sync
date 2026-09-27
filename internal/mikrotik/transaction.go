package mikrotik

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/Kfaraon/mikrotik-route-sync/internal/addresslist"
	"github.com/Kfaraon/mikrotik-route-sync/internal/config"
	"github.com/Kfaraon/mikrotik-route-sync/internal/storage"
)

// TransactionState описывает жизненный цикл транзакции.
type TransactionState string

const (
	StatePending    TransactionState = "pending"
	StateApplying   TransactionState = "applying"
	StateApplied    TransactionState = "applied"
	StateFailed     TransactionState = "failed"
	StateRolledBack TransactionState = "rolled_back"
	StateDegraded   TransactionState = "degraded"
)

// Transaction — атомарное применение изменений address-list одного сервиса.
// Паттерн "применить или откатиться" (компенсирующий откат через REST).
type Transaction struct {
	client     *Client
	cfg        *config.Config
	cache      *storage.Cache
	service    string
	log        *slog.Logger
	snapshotID string
	state      TransactionState
	startTime  time.Time
}

// NewTransaction создаёт транзакцию для сервиса.
func NewTransaction(client *Client, cfg *config.Config, cache *storage.Cache, service string, log *slog.Logger) *Transaction {
	return &Transaction{
		client:    client,
		cfg:       cfg,
		cache:     cache,
		service:   service,
		log:       log.With("service", service, "component", "transaction"),
		state:     StatePending,
		startTime: time.Now(),
	}
}

// SetSnapshotID привязывает ID снапшота для ручного восстановления.
func (t *Transaction) SetSnapshotID(id string) {
	t.snapshotID = id
}

// Apply применяет изменения: сначала добавление новых записей, затем удаление
// устаревших (PROMPT IV.4 — снижает риск временной потери покрытия).
// При ошибке на любом шаге выполняется компенсирующий откат; если откат
// частично неуспешен — состояние degraded.
func (t *Transaction) Apply(ctx context.Context, toAdd []addresslist.Entry, toRemove []addresslist.Entry) error {
	t.state = StateApplying
	list := t.cfg.Firewall.AddressList
	comment := addresslist.CommentFor(t.cfg.Firewall.CommentPrefix, t.service)
	t.log.Info("starting transaction",
		"list", list, "comment", comment,
		"add_count", len(toAdd), "remove_count", len(toRemove),
		"snapshot_id", t.snapshotID)

	var addedIDs []string
	var removed []addresslist.Entry
	var applyErr error

	// ФАЗА 1: добавление новых записей (перед удалением старых).
	for _, e := range toAdd {
		if err := ctx.Err(); err != nil {
			applyErr = fmt.Errorf("context cancelled during add phase: %w", err)
			break
		}
		e.List = list
		e.Comment = comment
		if !t.cfg.Firewall.ManageDisabled {
			e.Disabled = "false"
		}
		id, err := t.client.AddEntry(ctx, e)
		if err != nil {
			applyErr = fmt.Errorf("add entry %s: %w", e.Address, err)
			t.log.Error("failed to add entry during apply", "address", e.Address, "err", err)
			break
		}
		if id != "" {
			addedIDs = append(addedIDs, id)
		}
	}

	// ФАЗА 2: удаление устаревших записей (только если добавление удалось).
	if applyErr == nil {
		for _, e := range toRemove {
			if err := ctx.Err(); err != nil {
				applyErr = fmt.Errorf("context cancelled during remove phase: %w", err)
				break
			}
			if err := t.client.DeleteEntry(ctx, e.ID); err != nil {
				applyErr = fmt.Errorf("delete entry %s (%s): %w", e.ID, e.Address, err)
				t.log.Error("failed to delete entry during apply", "id", e.ID, "address", e.Address, "err", err)
				break
			}
			removed = append(removed, e)
		}
	}

	if applyErr != nil {
		t.state = StateFailed
		t.log.Error("transaction failed, initiating rollback",
			"apply_error", applyErr,
			"removed_before_failure", len(removed),
			"added_before_failure", len(addedIDs))

		if rbErr := t.rollback(ctx, addedIDs, removed); rbErr != nil {
			t.state = StateDegraded
			t.log.Error("rollback failed, service is DEGRADED",
				"apply_error", applyErr, "rollback_error", rbErr,
				"manual_intervention_required", true)
			return fmt.Errorf(
				"transaction failed (%v) AND rollback failed (%v): service %s is degraded, manual intervention required (snapshot_id: %s)",
				applyErr, rbErr, t.service, t.snapshotID)
		}

		t.state = StateRolledBack
		t.log.Info("rollback completed successfully", "rolled_back_deletes", len(removed), "rolled_back_adds", len(addedIDs))
		return fmt.Errorf("transaction failed and was rolled back: %w", applyErr)
	}

	t.state = StateApplied
	t.log.Info("transaction applied successfully",
		"added", len(addedIDs), "removed", len(removed),
		"duration", time.Since(t.startTime), "final_state", t.state)
	return nil
}

// State возвращает состояние транзакции.
func (t *Transaction) State() TransactionState { return t.state }

// SnapshotID возвращает ID привязанного снапшота.
func (t *Transaction) SnapshotID() string { return t.snapshotID }

// rollback: сначала восстанавливаем удалённое (PUT), затем удаляем добавленное.
// Порядок обратный применению. Best-effort: ошибки агрегируются.
func (t *Transaction) rollback(ctx context.Context, addedIDs []string, removed []addresslist.Entry) error {
	var rbErrs []error
	t.log.Info("rollback started", "to_delete", len(addedIDs), "to_restore", len(removed))

	list := t.cfg.Firewall.AddressList
	comment := addresslist.CommentFor(t.cfg.Firewall.CommentPrefix, t.service)

	for _, e := range removed {
		if err := ctx.Err(); err != nil {
			rbErrs = append(rbErrs, fmt.Errorf("context cancelled during rollback restore: %w", err))
			continue
		}
		e.ID = ""
		e.List = list
		e.Comment = comment
		if _, err := t.client.AddEntry(ctx, e); err != nil {
			rbErrs = append(rbErrs, fmt.Errorf("rollback restore entry %s: %w", e.Address, err))
			t.log.Error("rollback: failed to restore entry", "address", e.Address, "err", err)
		}
	}

	for _, id := range addedIDs {
		if err := ctx.Err(); err != nil {
			rbErrs = append(rbErrs, fmt.Errorf("context cancelled during rollback delete: %w", err))
			continue
		}
		if err := t.client.DeleteEntry(ctx, id); err != nil {
			rbErrs = append(rbErrs, fmt.Errorf("rollback delete entry %s: %w", id, err))
			t.log.Error("rollback: failed to delete added entry", "id", id, "err", err)
		}
	}

	if len(rbErrs) > 0 {
		return fmt.Errorf("%d rollback operations failed: %w", len(rbErrs), rbErrs[0])
	}
	return nil
}
